# How to use

## Install

```shell
go mod init test
go mod tidy
```

## Run

```shell
go run main.go scram_client.go -brokers "hostnam:port" -username "user_name" -passwd "password" -topic "topci_name" -tls -algorithm [sha256|sha512] -ca "path to ca.pem"
```
