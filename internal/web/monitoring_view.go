package web

import "serverwatch/internal/core"

// MonitoringView and its nested types moved to internal/core/dto.go (core.API
// contract task 2). See dashboard_view.go's equivalent note: these are Go
// type aliases, not new types, so every existing handler/template reference
// in this package keeps compiling unchanged.
type MonitoringView = core.MonitoringView
type MonitoringContainerView = core.MonitoringContainerView
type MonitoringUnitView = core.MonitoringUnitView
type MonitoringProcessView = core.MonitoringProcessView
type MonitoringDiskView = core.MonitoringDiskView
