package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"time"

	"github.com/containerd/containerd/pkg/atomic"

	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"k8s.io/klog/v2"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/bombsimon/logrusr/v4"
	"github.com/cenkalti/backoff/v4"
	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/castai/kvisor/blobscache"
	"github.com/castai/kvisor/castai"
	"github.com/castai/kvisor/castai/telemetry"
	"github.com/castai/kvisor/cloudscan/eks"
	"github.com/castai/kvisor/cloudscan/gke"
	"github.com/castai/kvisor/config"
	"github.com/castai/kvisor/controller"
	"github.com/castai/kvisor/delta"
	"github.com/castai/kvisor/imagescan"
	"github.com/castai/kvisor/jobsgc"
	"github.com/castai/kvisor/linters/kubebench"
	"github.com/castai/kvisor/linters/kubelinter"
	agentlog "github.com/castai/kvisor/log"
	"github.com/castai/kvisor/policy"
	"github.com/castai/kvisor/version"

	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

// These should be set via `go build` during a release.
var (
	GitCommit = "undefined"
	GitRef    = "no-ref"
	Version   = "local"
)

var (
	configPath = flag.String("config", "/etc/castai/config/config.yaml", "Config file path")
)

func main() {
	flag.Parse()

	logger := logrus.New()
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatal(err)
	}
	lvl, _ := logrus.ParseLevel(cfg.Log.Level)
	logger.SetLevel(lvl)

	binVersion := config.SecurityAgentVersion{
		GitCommit: GitCommit,
		GitRef:    GitRef,
		Version:   Version,
	}

	client := castai.NewClient(
		cfg.API.URL, cfg.API.Key,
		logger,
		cfg.API.ClusterID,
		"castai-kvisor",
		binVersion,
	)

	log := logrus.WithFields(logrus.Fields{})
	e := agentlog.NewExporter(logger, client, []logrus.Level{
		logrus.ErrorLevel,
		logrus.FatalLevel,
		logrus.PanicLevel,
		logrus.InfoLevel,
		logrus.WarnLevel,
	})

	logger.AddHook(e)
	logrus.RegisterExitHandler(e.Wait)

	ctx := signals.SetupSignalHandler()
	if err := run(ctx, logger, client, cfg, binVersion); err != nil {
		logErr := &logContextErr{}
		if errors.As(err, &logErr) {
			log = logger.WithFields(logErr.fields)
		}
		log.Fatalf("castai-kvisor failed: %v", err)
	}
}

func run(ctx context.Context, logger logrus.FieldLogger, castaiClient castai.Client, cfg config.Config, binVersion config.SecurityAgentVersion) (reterr error) {
	fields := logrus.Fields{}

	defer func() {
		if reterr == nil {
			return
		}
		reterr = &logContextErr{
			err:    reterr,
			fields: fields,
		}
	}()

	kubeConfig, err := retrieveKubeConfig(logger, cfg.KubeClient.KubeConfigPath)
	if err != nil {
		return err
	}

	kubeConfig.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(float32(cfg.KubeClient.QPS), cfg.KubeClient.Burst)

	clientSet, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	k8sVersion, err := version.Get(clientSet)
	if err != nil {
		return fmt.Errorf("getting kubernetes version: %w", err)
	}

	log := logger.WithFields(logrus.Fields{
		"version":     binVersion.Version,
		"k8s_version": k8sVersion.Full,
	})

	scanHandler := imagescan.NewScanHttpHandler(log, castaiClient)

	httpMux := http.NewServeMux()
	installPprofHandlers(httpMux)
	httpMux.Handle("/metrics", promhttp.Handler())
	httpMux.HandleFunc("/v1/image-scan/report", scanHandler.Handle)
	if cfg.ImageScan.Enabled {
		blobsCache := blobscache.NewServer(log, blobscache.ServerConfig{})
		blobsCache.RegisterHandlers(httpMux)
	}

	// Start http server for scan job, metrics and pprof handlers.
	go func() {
		httpAddr := fmt.Sprintf(":%d", cfg.HTTPPort)
		log.Infof("starting http server on %s", httpAddr)

		srv := &http.Server{
			Addr:         httpAddr,
			Handler:      httpMux,
			WriteTimeout: 5 * time.Second,
			ReadTimeout:  5 * time.Second,
		}
		if err := srv.ListenAndServe(); err != nil {
			log.Errorf("failed to start http server: %v", err)
		}
	}()

	log.Infof("running castai-kvisor version %v", binVersion)

	snapshotProvider := delta.NewSnapshotProvider()

	objectSubscribers := []controller.ObjectSubscriber{
		delta.NewSubscriber(
			log,
			log.Level,
			delta.Config{DeltaSyncInterval: cfg.DeltaSyncInterval},
			castaiClient,
			snapshotProvider,
			k8sVersion.MinorInt,
		),
	}

	telemetryManager := telemetry.NewManager(log, castaiClient)

	var scannedNodes []string
	telemetryResponse, err := castaiClient.PostTelemetry(ctx, true)
	if err != nil {
		log.Warnf("initial telemetry: %v", err)
	} else {
		cfg = telemetry.ModifyConfig(cfg, telemetryResponse)
		scannedNodes = telemetryResponse.NodeIDs
	}

	linter, err := kubelinter.New(lo.Keys(castai.LinterRuleMap))
	if err != nil {
		return fmt.Errorf("setting up linter: %w", err)
	}

	policyEnforcer := policy.NewEnforcer(linter, cfg.PolicyEnforcement)
	telemetryManager.AddObservers(policyEnforcer.TelemetryObserver())

	if cfg.Linter.Enabled {
		log.Info("linter enabled")
		linterSub, err := kubelinter.NewSubscriber(log, castaiClient, linter)
		if err != nil {
			return err
		}
		objectSubscribers = append(objectSubscribers, linterSub)
	}
	if cfg.KubeBench.Enabled {
		log.Info("kubebench enabled")
		if cfg.KubeBench.Force {
			scannedNodes = []string{}
		}
		podLogReader := agentlog.NewPodLogReader(clientSet)
		objectSubscribers = append(objectSubscribers, kubebench.NewSubscriber(
			log,
			clientSet,
			cfg.PodNamespace,
			cfg.Provider,
			cfg.KubeBench.ScanInterval,
			castaiClient,
			podLogReader,
			scannedNodes,
		))
	}
	if cfg.ImageScan.Enabled {
		log.Info("imagescan enabled")
		imgScanSubscriber := imagescan.NewSubscriber(
			log,
			cfg.ImageScan,
			imagescan.NewImageScanner(clientSet, cfg),
			castaiClient,
			k8sVersion.MinorInt,
		)
		objectSubscribers = append(objectSubscribers, imgScanSubscriber)
	}

	if len(objectSubscribers) == 0 {
		return errors.New("no subscribers enabled")
	}

	if cfg.CloudScan.Enabled {
		switch cfg.Provider {
		case "gke":
			gkeCloudScanner, err := gke.NewScanner(log, cfg.CloudScan, cfg.ImageScan.Enabled, castaiClient)
			if err != nil {
				return err
			}
			go gkeCloudScanner.Start(ctx)
		case "eks":
			awscfg, err := awsconfig.LoadDefaultConfig(ctx)
			if err != nil {
				return err
			}

			go eks.NewScanner(log, cfg.CloudScan, awseks.NewFromConfig(awscfg), castaiClient).Start(ctx)
		}
	}

	gc := jobsgc.NewGC(log, clientSet, jobsgc.Config{
		CleanupInterval: 10 * time.Minute,
		CleanupJobAge:   10 * time.Minute,
		Namespace:       cfg.PodNamespace,
	})
	go gc.Start(ctx)

	informersFactory := informers.NewSharedInformerFactory(clientSet, 0)
	ctrl := controller.New(log, informersFactory, objectSubscribers, k8sVersion)

	resyncObserver := delta.ResyncObserver(ctx, log, snapshotProvider, castaiClient)
	telemetryManager.AddObservers(resyncObserver)
	featureObserver, featuresCtx := telemetry.ObserveDisabledFeatures(ctx, cfg, log)
	telemetryManager.AddObservers(featureObserver)

	go telemetryManager.Run(ctx)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)

	logr := logrusr.New(logger)
	klog.SetLogger(logr)

	mngr, err := manager.New(kubeConfig, manager.Options{
		Logger:                  logr.WithName("manager"),
		Port:                    cfg.ServicePort,
		CertDir:                 cfg.CertsDir,
		NewCache:                cache.New,
		Scheme:                  scheme,
		MetricsBindAddress:      "0",
		HealthProbeBindAddress:  ":" + strconv.Itoa(cfg.StatusPort),
		LeaderElection:          cfg.LeaderElection,
		LeaderElectionID:        cfg.ServiceName,
		LeaderElectionNamespace: cfg.PodNamespace,
		MapperProvider: func(c *rest.Config) (meta.RESTMapper, error) {
			return apiutil.NewDynamicRESTMapper(c)
		},
	})
	if err != nil {
		return fmt.Errorf("setting up manager: %w", err)
	}

	if err := mngr.AddHealthzCheck("default", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mngr.AddReadyzCheck("default", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	if cfg.PolicyEnforcement.Enabled {
		rotatorReady := make(chan struct{})
		err = rotator.AddRotator(mngr, &rotator.CertRotator{
			SecretKey: types.NamespacedName{
				Name:      cfg.CertsSecret,
				Namespace: cfg.PodNamespace,
			},
			CertDir:        cfg.CertsDir,
			CAName:         "kvisor",
			CAOrganization: "cast.ai",
			DNSName:        fmt.Sprintf("%s.%s.svc", cfg.ServiceName, cfg.PodNamespace),
			IsReady:        rotatorReady,
			Webhooks: []rotator.WebhookInfo{
				{
					Name: cfg.PolicyEnforcement.WebhookName,
					Type: rotator.Validating,
				},
			},
		})
		if err != nil {
			return fmt.Errorf("setting up cert rotation: %w", err)
		}

		ready := atomic.NewBool(false)
		if err := mngr.AddReadyzCheck("webhook", func(req *http.Request) error {
			if !ready.IsSet() {
				return errors.New("webhook is not ready yet")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("add readiness check: %w", err)
		}

		go func() {
			<-rotatorReady
			mngr.GetWebhookServer().Register("/validate", &admission.Webhook{
				Handler: policyEnforcer,
			})
			ready.Set()
		}()
	}

	// Does the work. Blocks.
	return ctrl.Run(featuresCtx, mngr)
}

func retrieveKubeConfig(log logrus.FieldLogger, kubepath string) (*rest.Config, error) {
	if kubepath != "" {
		data, err := os.ReadFile(kubepath)
		if err != nil {
			return nil, fmt.Errorf("reading kubeconfig at %s: %w", kubepath, err)
		}
		restConfig, err := clientcmd.RESTConfigFromKubeConfig(data)
		if err != nil {
			return nil, fmt.Errorf("building rest config from kubeconfig at %s: %w", kubepath, err)
		}
		log.Debug("using kubeconfig from env variables")
		return restConfig, nil
	}

	inClusterConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	inClusterConfig.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &kubeRetryTransport{
			log:           log,
			next:          rt,
			maxRetries:    10,
			retryInterval: 3 * time.Second,
		}
	})
	log.Debug("using in cluster kubeconfig")
	return inClusterConfig, nil
}

func installPprofHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

type kubeRetryTransport struct {
	log           logrus.FieldLogger
	next          http.RoundTripper
	maxRetries    uint64
	retryInterval time.Duration
}

func (rt *kubeRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	err := backoff.RetryNotify(func() error {
		var err error
		resp, err = rt.next.RoundTrip(req) //nolint:bodyclose
		if err != nil {
			// Previously client-go contained logic to retry connection refused errors. See https://github.com/kubernetes/kubernetes/pull/88267/files
			if net.IsConnectionRefused(err) {
				return err
			}
			return backoff.Permanent(err)
		}
		return nil
	}, backoff.WithMaxRetries(backoff.NewConstantBackOff(rt.retryInterval), rt.maxRetries),
		func(err error, duration time.Duration) {
			if err != nil {
				rt.log.Warnf("kube api server connection refused, will retry: %v", err)
			}
		})
	return resp, err
}

type logContextErr struct {
	err    error
	fields logrus.Fields
}

func (e *logContextErr) Error() string {
	return e.err.Error()
}

func (e *logContextErr) Unwrap() error {
	return e.err
}
