#!/bin/bash

source ../common.sh

disconnect_docker_hosts l3h1 llb1
disconnect_docker_hosts l3ep1 llb1

delete_docker_host llb1
delete_docker_host l3h1
delete_docker_host l3ep1

# The store runs with --rm; rm -f stops it and the daemon removes it.
docker rm -f pg-mcp >/dev/null 2>&1

rm -rf loxilb-mcp loxilb-mcp-ci.yaml .mcp-audit llb1_config

echo "#########################################"
echo "Deleted testbed"
echo "#########################################"
