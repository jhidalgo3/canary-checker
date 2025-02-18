#!/bin/bash

# The script tests the push subcommand as well as postgres convectivity for canary-checker.

set -e

export KUBECONFIG=~/.kube/config
export KARINA="karina -c $(pwd)/test/karina.yaml"
export DOCKER_API_VERSION=1.39
export CLUSTER_NAME=kind-test
export PATH=$(pwd)/.bin:$PATH
export ROOT=$(pwd)

echo "::group::Provisioning"
if [[ ! -e .certs/root-ca.key ]]; then
$KARINA ca generate --name root-ca --cert-path .certs/root-ca.crt --private-key-path .certs/root-ca.key --password foobar  --expiry 1
$KARINA ca generate --name ingress-ca --cert-path .certs/ingress-ca.crt --private-key-path .certs/ingress-ca.key --password foobar  --expiry 1
$KARINA ca generate --name sealed-secrets --cert-path .certs/sealed-secrets-crt.pem --private-key-path .certs/sealed-secrets-key.pem --password foobar  --expiry 1
fi

## starting the postgres as docker container
docker run --rm -p 5432:5432  --name some-postgres -e POSTGRES_PASSWORD=mysecretpassword -d  postgres
if $KARINA provision kind-cluster -e name=$CLUSTER_NAME -v ; then
echo "::endgroup::"
else
echo "::endgroup::"
exit 1
fi

kubectl config use-context kind-$CLUSTER_NAME

echo "::group::Deploying Base"
## applying CRD and a sample fixture for the operator
kubectl apply -f config/deploy/crd.yaml
## FIXME: kubectl wait for condition on CRD
# kubectl wait --for condition=established --timeout=60s crd/canaries.canaries.flanksource.com
sleep 10
echo "::endgroup::"


echo "::group::Operator"
## starting operator in background
go run main.go operator -vvv --db="postgres://postgres:mysecretpassword@localhost:5432/postgres" --maxStatusCheckCount=1 &
PROC_ID=$!

## sleeping for a bit to let the operator start and statuses to be present
sleep 240

curl http://0.0.0.0:8080/api


i=0
while [ $i -lt 5 ]
do
    go run main.go push http://0.0.0.0:8080 --name abc --description a --type junit --status passed --duration 10ms --message "10 of 10 passed"
    i=$((i+1))
done


CANARY_COUNT=$(kubectl get canaries.canaries.flanksource.com -A --no-headers | wc -l)
CANARY_COUNT=$(echo "$CANARY_COUNT" | xargs)
STATUS_COUNT_POSTGRES=$(curl -s http://0.0.0.0:8080/api\?count\=4  | jq ."checks[0].checkStatuses | length")
STATUS_COUNT_MEMORY=$(curl -s http://0.0.0.0:8080/api  | jq ."checks[0].checkStatuses | length")



echo "Canary count: ${CANARY_COUNT}"
echo "Postgres count: ${STATUS_COUNT_POSTGRES}"
echo "Memory count: ${STATUS_COUNT_MEMORY}"


if [ "${CANARY_COUNT}" -gt 0 ]; then 
    echo "Number of canaries is greater than 0: ${CANARY_COUNT}"
    exit 1
fi

if [ "${STATUS_COUNT_MEMORY}" -gt 1 ]; then
    echo "Status in memory should not be greater than 1"
    sudo kill -9 $PROC_ID || :
    exit 1
fi

if [ "${STATUS_COUNT_POSTGRES}" -ge 4 ]; then
    sudo kill -9 $PROC_ID || :
    echo "::endgroup::"
    exit 0
else
    echo "expected statuses length to be greater than 2 but got ${STATUS_COUNT_POSTGRES}"
    sudo kill -9 $PROC_ID || :
    echo "::endgroup::"
    exit 1
fi
