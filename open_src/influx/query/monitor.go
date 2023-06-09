package query

/*

Copyright (c) 2018 InfluxData
This code is originally from: https://github.com/influxdata/influxdb/blob/1.7/query/monitor.go

2022.01.23 Remove unused function:PointLimitMonitor.
Huawei Cloud Computing Technologies Co., Ltd.

*/

import (
	"context"
)

// MonitorFunc is a function that will be called to check if a query
// is currently healthy. If the query needs to be interrupted for some reason,
// the error should be returned by this function.
type MonitorFunc func(<-chan struct{}) error

// Monitor monitors the status of a query and returns whether the query should
// be aborted with an error.
type Monitor interface {
	// Monitor starts a new goroutine that will monitor a query. The function
	// will be passed in a channel to signal when the query has been finished
	// normally. If the function returns with an error and the query is still
	// running, the query will be terminated.
	Monitor(fn MonitorFunc)
}

// MonitorFromContext returns a Monitor embedded within the Context
// if one exists.
func MonitorFromContext(ctx context.Context) Monitor {
	v, _ := ctx.Value(monitorContextKey{}).(Monitor)
	return v
}
