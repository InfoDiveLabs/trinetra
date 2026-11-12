package trinetra

import "time"

// companionSpec describes one supervised companion process.
type companionSpec struct {
	Name    string
	Resolve func() (string, error)
	Env     []string
	Start   func(path string, env []string) (supervisedProc, error)
	OnExit  func(exitErr error, uptime time.Duration) (retry bool)
	Logf    func(format string, args ...any)
}

// firstPartyCompanionNames returns the bundled companions ("ctl", "web").
func firstPartyCompanionNames() []string { return []string{"ctl", "web"} }

// superviseCompanion resolves and spawns the companion before every attempt, restarts it with
// capped backoff until stopCh closes or OnExit returns false, and resets backoff after webBackoffReset.
func superviseCompanion(spec companionSpec, stopCh <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	logf := spec.Logf
	if logf == nil {
		logf = func(format string, args ...any) { supervisorLog(format, args...) }
	}
	bin := "trinetra-" + spec.Name

	backoff := webBackoffMin
	for {
		select {
		case <-stopCh:
			return
		default:
		}

		path, err := spec.Resolve()
		if err != nil {
			logf("%s: refusing to start %s: %v (run `trinetra install` after a rebuild)", spec.Name, bin, err)
			if !sleepOrStop(backoff, stopCh) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		proc, err := spec.Start(path, spec.Env)
		if err != nil {
			logf("%s: failed to spawn %s: %v", spec.Name, path, err)
			if !sleepOrStop(backoff, stopCh) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		start := timeNow()
		waitCh := make(chan error, 1)
		go func() { waitCh <- proc.Wait() }()
		select {
		case <-stopCh:
			_ = proc.Kill()
			<-waitCh
			return
		case werr := <-waitCh:
			logf("%s: %s exited: %v", spec.Name, bin, werr)
			uptime := timeNow().Sub(start)
			if spec.OnExit != nil && !spec.OnExit(werr, uptime) {
				return
			}
			if uptime >= webBackoffReset {
				backoff = webBackoffMin
			}
			if !sleepOrStop(backoff, stopCh) {
				return
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// startCompanion runs superviseCompanion in a goroutine and returns a blocking stop func.
func startCompanion(spec companionSpec) (stop func()) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go superviseCompanion(spec, stopCh, done)
	return func() {
		close(stopCh)
		<-done
	}
}
