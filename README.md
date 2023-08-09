# port-scan-exporter
This is a simple prometheus exporter that periodically scan pods in the cluster (not using HostNetwork) for open TCP ports.  
The scans happen concurrently, using a configurable number of goroutine per each pod.

## Installing
A helm chart is provided in the chart directory.  
Simply run
```shell
helm package
```
in the chart directory to generate the package. You can then install the chart as usual, e.g.:
```shell
helm upgrade --install port-scan-exporter chart/port-scan-exporter-0.1.0.tgz --values my_custom_values.yaml
```

Refer to the values.yaml file in the chart subdirectory for the default values.
