package plugin

import "errors"

var (
	ErrUnavailable          = errors.New("plugin: subsystem not available")
	ErrNotFound             = errors.New("plugin: not installed")
	ErrConsentRequired      = errors.New("plugin: consent required")
	ErrUnverifiedDisallowed = errors.New("plugin: unverified plugins are disabled by plugins.allow_unverified")
	ErrIncompatible         = errors.New("plugin: incompatible host_api")
	ErrRevoked              = errors.New("plugin: revoked by the registry")
	ErrPermissionDenied     = errors.New("plugin: permission denied")
	ErrDeadline             = errors.New("plugin: call deadline exceeded")
	ErrTooLarge             = errors.New("plugin: message too large")
	ErrBusy                 = errors.New("plugin: too many calls in flight")
	ErrNotRunning           = errors.New("plugin: not running")
	ErrBadSignature         = errors.New("plugin: signature does not verify")
	ErrNoRegistryKeys       = errors.New("plugin: registry not configured in this build")
	ErrCatalogStale         = errors.New("plugin: catalog is stale; refresh before installing registry plugins")
)

// Sentinels lists every error above, for control's wire-error reconstruction.
var Sentinels = []error{ErrUnavailable, ErrNotFound, ErrConsentRequired, ErrUnverifiedDisallowed, ErrIncompatible,
	ErrRevoked, ErrPermissionDenied, ErrDeadline, ErrTooLarge, ErrBusy, ErrNotRunning, ErrBadSignature,
	ErrNoRegistryKeys, ErrCatalogStale}
