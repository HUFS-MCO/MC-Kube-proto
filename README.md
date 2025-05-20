# MC-Kube-proto
Software Defined Vehicle(SDV)를 위한 Mixed Criticality aware Orchestration

operator-sdk init --domain sdv.com --repo mc-kube

CGO_ENABLED=0 operator-sdk create api --group mcoperator --version v1 --kind McKube --resource --controller

resources=mckuberealtimes
