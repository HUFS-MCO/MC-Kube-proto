# MC-Kube-proto
Software Defined Vehicle(SDV)를 위한 Mixed Criticality aware Orchestration

operator-sdk init --domain sdv.com --repo mc-kube

CGO_ENABLED=0 operator-sdk create api --group mcoperator --version v1 --kind McKube --resource --controller

resources=mckuberealtimes

pod.Labels["sdv.com"]]

dockerhub: https://hub.docker.com/repository/docker/woya031/mckube/general
