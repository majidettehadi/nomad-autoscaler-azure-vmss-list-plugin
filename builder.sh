#!/usr/bin/env bash

CGO_ENABLED=0

go build -tags netgo -o azure-vmss-list .