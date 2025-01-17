#!/bin/bash

set -ex

KIND_CONTEXT="${KIND_CONTEXT:-kind}"
GOARCH="$(go env GOARCH)"

if [ "$IMAGE_TAG" == "" ]
then
  echo "env variable IMAGE_TAG is required"
  exit 1
fi

# Build e2e docker image.
GOOS=linux GOARCH=$GOARCH CGO_ENABLED=0 go build -o bin/kvisor-e2e ./e2e
docker build . -t kvisor-e2e:local --build-arg image_tag=$IMAGE_TAG -f Dockerfile.e2e

# Load local image into kind.
kind load docker-image kvisor-e2e:local --name $KIND_CONTEXT

if [ "$IMAGE_TAG" == "local" ]
then
  GOOS=linux GOARCH=$GOARCH CGO_ENABLED=0 go build -o bin/castai-kvisor-$GOARCH ./cmd/kvisor
  docker build . -t kvisor:local -f Dockerfile
  kind load docker-image kvisor:local --name $KIND_CONTEXT
fi

# Deploy e2e resources.
function printJobLogs() {
  echo "Jobs:"
  kubectl get jobs -owide
  echo "Pods:"
  kubectl get pods -owide
  echo "E2E Job logs:"
  kubectl logs -l job-name=e2e --tail=-1
}
trap printJobLogs EXIT

ns="castai-kvisor-e2e"
kubectl delete ns $ns --force || true
kubectl create ns $ns || true
kubectl config set-context --current --namespace=$ns
kubectl apply -f ./e2e/e2e.yaml -n $ns
echo "Waiting for job to finish"

i=0
sleep_seconds=5
retry_count=20
while true; do
  if [ "$i" == "$retry_count" ];
  then
    echo "Timeout waiting for job to complete"
    exit 1
  fi

  if kubectl wait --for=condition=complete --timeout=0s job/e2e 2>/dev/null; then
    job_result=0
    break
  fi

  if kubectl wait --for=condition=failed --timeout=0s job/e2e 2>/dev/null; then
    job_result=1
    break
  fi

  sleep $sleep_seconds
  echo "Job logs:"
  kubectl logs -l job-name=e2e --since=5s
  i=$((i+1))
done

if [[ $job_result -eq 1 ]]; then
    echo "Job failed!"
    exit 1
fi
echo "Job succeeded!"
