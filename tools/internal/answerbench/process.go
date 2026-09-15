package answerbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

type managedProcess struct {
	cmd         *exec.Cmd
	done        chan struct{}
	mu          sync.Mutex
	err         error
	stopping    bool
	unexpected  bool
	releaseTree func() error
	releaseOnce sync.Once
	releaseErr  error
}

func startManaged(cmd *exec.Cmd) (*managedProcess, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	releaseTree, err := ownProcessTree(cmd)
	if err != nil {
		killErr := killChild(cmd)
		waitErr := cmd.Wait()
		return nil, errors.Join(err, killErr, waitErr)
	}
	p := &managedProcess{cmd: cmd, done: make(chan struct{}), releaseTree: releaseTree}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.err = err
		p.unexpected = !p.stopping
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func (p *managedProcess) UnexpectedExit() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unexpected
}

func (p *managedProcess) Done() <-chan struct{} { return p.done }

func (p *managedProcess) Err() error {
	select {
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.err
	default:
		return nil
	}
}

func (p *managedProcess) stop(cancel context.CancelFunc) error {
	p.mu.Lock()
	stopErr := terminateChild(p.cmd)
	if stopErr == nil {
		p.stopping = true
	}
	p.mu.Unlock()
	cancel()
	if errors.Is(stopErr, os.ErrProcessDone) {
		stopErr = nil
	}
	select {
	case <-p.done:
		return errors.Join(wrapProcessError("terminate child process", stopErr), wrapProcessError("kill child process tree", killChild(p.cmd)), wrapProcessError("release child process tree", p.releaseOwnedTree()))
	case <-time.After(4 * time.Second):
	}
	killErr := killChild(p.cmd)
	releaseErr := p.releaseOwnedTree()
	if killErr != nil || releaseErr != nil {
		return errors.Join(wrapProcessError("terminate child process", stopErr), wrapProcessError("kill child process tree", killErr), wrapProcessError("release child process tree", releaseErr))
	}
	select {
	case <-p.done:
		return errors.Join(wrapProcessError("terminate child process", stopErr), wrapProcessError("kill child process tree", killChild(p.cmd)))
	case <-time.After(time.Second):
		return errors.Join(wrapProcessError("terminate child process", stopErr), errors.New("child process did not exit after kill"))
	}
}

func wrapProcessError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

func (p *managedProcess) releaseOwnedTree() error {
	p.releaseOnce.Do(func() {
		if p.releaseTree != nil {
			p.releaseErr = p.releaseTree()
		}
	})
	return p.releaseErr
}
