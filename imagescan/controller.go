package imagescan

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/castai/kvisor/castai"
	imgcollectorconfig "github.com/castai/kvisor/cmd/kvisor/imgcollector/config"
	"github.com/castai/kvisor/config"
	"github.com/castai/kvisor/kube"
	"github.com/castai/kvisor/metrics"
)

type castaiClient interface {
	SendImageMetadata(ctx context.Context, meta *castai.ImageMetadata) error
	GetSyncState(ctx context.Context, filter *castai.SyncStateFilter) (*castai.SyncStateResponse, error)
	UpdateImageStatus(ctx context.Context, report *castai.UpdateImagesStatusRequest) error
}

func NewController(
	log logrus.FieldLogger,
	cfg config.ImageScan,
	imageScanner imageScanner,
	client castaiClient,
	k8sVersionMinor int,
	kubeController kubeController,
) *Controller {
	ctx, cancel := context.WithCancel(context.Background())
	log = log.WithField("component", "imagescan")
	return &Controller{
		ctx:               ctx,
		cancel:            cancel,
		imageScanner:      imageScanner,
		client:            client,
		kubeController:    kubeController,
		delta:             newDeltaState(kubeController),
		log:               log,
		cfg:               cfg,
		k8sVersionMinor:   k8sVersionMinor,
		timeGetter:        timeGetter(),
		initialScansDelay: cfg.InitDelay,
	}
}

func timeGetter() func() time.Time {
	return func() time.Time {
		return time.Now().UTC()
	}
}

type Controller struct {
	ctx             context.Context
	cancel          context.CancelFunc
	delta           *deltaState
	imageScanner    imageScanner
	client          castaiClient
	kubeController  kubeController
	log             logrus.FieldLogger
	cfg             config.ImageScan
	k8sVersionMinor int
	timeGetter      func() time.Time

	initialScansDelay time.Duration
	fullSnapshotSent  bool
}

func (s *Controller) RequiredInformers() []reflect.Type {
	rt := []reflect.Type{
		reflect.TypeOf(&corev1.Pod{}),
		reflect.TypeOf(&corev1.Node{}),
	}
	return rt
}

func (s *Controller) Run(ctx context.Context) error {
	// Before starting normal scans and deltas processing
	// we need to spend some time processing only deltas to make sure
	// we have full images view.
	if err := s.waitInitialDeltaQueueSync(ctx); err != nil {
		return err
	}

	scanTicker := time.NewTicker(s.cfg.ScanInterval)
	defer scanTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case deltaItem := <-s.delta.queue:
			s.handleDelta(deltaItem.event, deltaItem.obj)
		case <-scanTicker.C:
			if err := s.scheduleScans(ctx); err != nil {
				s.log.Errorf("images scan failed: %v", err)
			}
		}
	}
}

func (s *Controller) waitInitialDeltaQueueSync(ctx context.Context) error {
	waitTimeout := time.After(s.initialScansDelay)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case deltaItem := <-s.delta.queue:
			s.handleDelta(deltaItem.event, deltaItem.obj)
		case <-waitTimeout:
			return nil
		}
	}
}

func (s *Controller) OnAdd(obj kube.Object) {
	s.delta.queue <- deltaQueueItem{
		event: kube.EventAdd,
		obj:   obj,
	}
}

func (s *Controller) OnUpdate(obj kube.Object) {
	s.delta.queue <- deltaQueueItem{
		event: kube.EventUpdate,
		obj:   obj,
	}
}

func (s *Controller) OnDelete(obj kube.Object) {
	s.delta.queue <- deltaQueueItem{
		event: kube.EventDelete,
		obj:   obj,
	}
}

func (s *Controller) handleDelta(event kube.Event, o kube.Object) {
	switch event {
	case kube.EventAdd, kube.EventUpdate:
		s.delta.upsert(o)
	case kube.EventDelete:
		s.delta.delete(o)
	}
}

func (s *Controller) scheduleScans(ctx context.Context) (rerr error) {
	s.syncFromRemoteState(ctx)

	if err := s.updateImageStatuses(ctx); err != nil {
		s.log.Errorf("sending images resources changes: %v", err)
	}

	// Scan pending images.
	pendingImages := s.findPendingImages()
	concurrentScans := s.concurrentScansNumber()
	imagesForScan := pendingImages
	if len(imagesForScan) > concurrentScans {
		imagesForScan = imagesForScan[:concurrentScans]
	}
	if l := len(imagesForScan); l > 0 {
		s.log.Infof("scheduling %d images scans", l)
		if err := s.scanImages(ctx, imagesForScan); err != nil {
			return err
		}
		s.log.Info("images scan finished")
	} else {
		s.log.Debug("skipping images scan, no pending images")
	}

	return nil
}

func (s *Controller) findPendingImages() []*image {
	images := s.delta.getImages()

	now := s.timeGetter()

	privateImagesCount := lo.CountBy(images, func(v *image) bool {
		return isImagePrivate(v)
	})
	pendingImages := lo.Filter(images, func(v *image, _ int) bool {
		return isImagePending(v, now)
	})
	sort.Slice(pendingImages, func(i, j int) bool {
		return pendingImages[i].failures < pendingImages[j].failures
	})
	s.log.Infof("found %d images, pending images %d", len(images), len(pendingImages))
	metrics.SetTotalImagesCount(len(images))
	metrics.SetPendingImagesCount(len(pendingImages))
	if privateImagesCount > 0 {
		s.log.Warnf("skipping %d private images", privateImagesCount)
	}
	return pendingImages
}

func (s *Controller) scanImages(ctx context.Context, images []*image) error {
	var wg sync.WaitGroup
	for _, img := range images {
		if img.name == "" {
			return fmt.Errorf("no image name set, image_id=%s", img.id)
		}

		wg.Add(1)
		go func(img *image) {
			defer wg.Done()

			if ctx.Err() != nil {
				return
			}

			ctx, cancel := context.WithTimeout(ctx, s.cfg.ScanTimeout)
			defer cancel()

			log := s.log.WithField("image", img.name)
			log.Info("scanning image")
			if err := s.scanImage(ctx, img); err != nil {
				log.Errorf("image scan failed: %v", err)
				parsedErr := parseErrorFromLog(err)
				s.delta.setImageScanError(img, parsedErr)
				if err := s.updateImageStatusAsFailed(ctx, img, parsedErr); err != nil {
					s.log.Errorf("sending images resources changes: %v", err)
				}
				return
			}
			log.Info("image scan finished")
			s.delta.updateImage(img, func(i *image) { i.scanned = true })
		}(img)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Controller) findBestNodeAndMode(img *image) (string, string, error) {
	mode := s.cfg.Mode
	if img.lastScanErr != nil && errors.Is(img.lastScanErr, errImageScanLayerNotFound) {
		// Fallback to remote if previously it failed due to missing layers.
		s.log.Debugf("selecting remote mode because of lastScanErr")
		mode = string(imgcollectorconfig.ModeRemote)
	}

	var nodeNames []string
	if imgcollectorconfig.Mode(mode) == imgcollectorconfig.ModeHostFS {
		// In HostFS we need to choose only from nodes which contains this image.
		nodeNames = lo.Keys(img.nodes)
		if len(nodeNames) == 0 {
			return "", "", errors.New("image with empty nodes")
		}

		nodeNames = s.delta.filterCastAIManagedNodes(nodeNames)
		if len(nodeNames) == 0 {
			// If image is not running on CAST AI managed nodes fallback to remote scan.
			mode = string(imgcollectorconfig.ModeRemote)
			s.log.Debugf("selecting remote mode because no CAST AI managed nodes found")
			nodeNames = lo.Keys(s.delta.nodes)
		}
	} else {
		nodeNames = lo.Keys(s.delta.nodes)
	}

	// skipping non-linux nodes as they are not supported as for today
	nodeNames = s.filterWindowsNodes(nodeNames)

	// Resolve best node.
	memQty := resource.MustParse(s.cfg.MemoryRequest)
	cpuQty := resource.MustParse(s.cfg.CPURequest)
	resolvedNode, err := s.delta.findBestNode(nodeNames, memQty.AsDec(), cpuQty.AsDec())
	if err != nil {
		if errors.Is(err, errNoCandidates) && imgcollectorconfig.Mode(mode) == imgcollectorconfig.ModeHostFS {
			// if mode was host fs fallback to remote scan and try picking node again.
			mode = string(imgcollectorconfig.ModeRemote)
			s.log.Debugf("selecting a node in remote mode because of errNoCandidates")
			nodeNames = lo.Keys(s.delta.nodes)
			resolvedNode, err = s.delta.findBestNode(nodeNames, memQty.AsDec(), cpuQty.AsDec())
			if err != nil {
				return "", "", err
			}
		} else {
			return "", "", err
		}
	}

	return resolvedNode, mode, nil
}

func (s *Controller) filterWindowsNodes(names []string) []string {
	var filtered []string
	for _, name := range names {
		if s.delta.nodes[name].os != "linux" {
			s.log.Debugf("skipping non-linux node %s", name)
			continue
		}
		filtered = append(filtered, name)
	}
	return filtered
}

func (s *Controller) scanImage(ctx context.Context, img *image) (rerr error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	node, mode, err := s.findBestNodeAndMode(img)
	if err != nil {
		return err
	}

	start := time.Now()
	defer func() {
		metrics.IncScansTotal(metrics.ScanTypeImage, rerr)
		metrics.ObserveScanDuration(metrics.ScanTypeImage, start)
	}()

	collectorImageDetails, found := s.kubeController.GetKvisorImageDetails()
	if !found {
		return errors.New("kvisor image details not found")
	}

	return s.imageScanner.ScanImage(ctx, ScanImageParams{
		ImageName:                   img.name,
		ImageID:                     img.id,
		ContainerRuntime:            string(img.containerRuntime),
		Mode:                        mode,
		ResourceIDs:                 lo.Keys(img.owners),
		NodeName:                    node,
		DeleteFinishedJob:           true,
		WaitForCompletion:           true,
		WaitDurationAfterCompletion: 30 * time.Second,
		Architecture:                img.architecture,
		Os:                          img.os,
		CollectorImageDetails:       collectorImageDetails,
	})
}

func (s *Controller) concurrentScansNumber() int {
	if s.delta.nodeCount() == 1 {
		return 1
	}

	return int(s.cfg.MaxConcurrentScans)
}

func (s *Controller) updateImageStatuses(ctx context.Context) error {
	images := s.delta.getImages()
	if s.fullSnapshotSent {
		images = lo.Filter(images, func(item *image, index int) bool {
			return item.ownerChangedAt.After(item.resourcesUpdatedAt)
		})
	}
	if len(images) == 0 {
		return nil
	}
	now := s.timeGetter()
	var imagesChanges []castai.Image
	for _, img := range images {
		resourceIds := lo.Keys(img.owners)

		var updatedStatus castai.ImageScanStatus
		if isImagePending(img, now) {
			updatedStatus = castai.ImageScanStatusPending
		}
		imagesChanges = append(imagesChanges, castai.Image{
			ID:           img.id,
			Architecture: img.architecture,
			ResourcesChange: castai.ResourcesChange{
				ResourceIDs: resourceIds,
			},
			ImageName: img.name,
			Status:    updatedStatus,
		})
	}

	s.log.Info("sending images resources changes")
	report := &castai.UpdateImagesStatusRequest{
		FullSnapshot: s.fullSnapshotSent,
		Images:       imagesChanges,
	}
	err := s.client.UpdateImageStatus(ctx, report)
	if err != nil {
		return err
	}
	for _, img := range images {
		img.resourcesUpdatedAt = now
	}
	s.fullSnapshotSent = true
	return nil
}

func (s *Controller) updateImageStatusAsFailed(ctx context.Context, image *image, scanJobError error) error {
	if image == nil {
		return errors.New("image is missing")
	}
	var errorMsg string
	if scanJobError != nil {
		errorMsg = scanJobError.Error()
	}

	updatedImage := castai.Image{
		ID:           image.id,
		ImageName:    image.name,
		Architecture: image.architecture,
		Status:       castai.ImageScanStatusError,
		ErrorMsg:     errorMsg,
	}

	s.log.Info("sending image failed status")
	report := &castai.UpdateImagesStatusRequest{
		Images: []castai.Image{updatedImage},
	}

	return s.client.UpdateImageStatus(ctx, report)
}

func (s *Controller) syncFromRemoteState(ctx context.Context) {
	images := s.delta.getImages()
	now := s.timeGetter().UTC()
	imagesWithNotSyncedState := lo.Filter(images, func(item *image, index int) bool {
		return !item.scanned && item.lastRemoteSyncAt.Before(now.Add(-10*time.Minute))
	})

	if len(imagesWithNotSyncedState) == 0 {
		return
	}

	imagesIds := lo.Map(imagesWithNotSyncedState, func(item *image, index int) string {
		return item.id
	})
	s.log.Debugf("sync images state from remote")
	resp, err := s.client.GetSyncState(ctx, &castai.SyncStateFilter{ImagesIds: imagesIds})
	if err != nil {
		s.log.Errorf("getting images sync state from remote: %v", err)
		return
	}
	if resp.Images == nil {
		return
	}

	// Set sync state for all these images to prevent constant api calls.
	for _, img := range imagesWithNotSyncedState {
		img.lastRemoteSyncAt = now
	}
	// Set images as scanned from remote response.
	for _, scannedImage := range resp.Images.ScannedImages {
		s.delta.setImageScanned(scannedImage)
	}

	// If full resources resync is required it will be sent during next scheduled scan.
	if resp.Images.FullResourcesResyncRequired {
		s.fullSnapshotSent = false
	}
	s.log.Infof("images updated from remote state, full_resync=%v, scanned_images=%d", resp.Images.FullResourcesResyncRequired, len(resp.Images.ScannedImages))
}

func isImagePending(v *image, now time.Time) bool {
	return !v.scanned &&
		len(v.owners) > 0 &&
		!isImagePrivate(v) &&
		(v.nextScan.IsZero() || v.nextScan.Before(now))
}

func isImagePrivate(v *image) bool {
	return errors.Is(v.lastScanErr, errPrivateImage)
}
