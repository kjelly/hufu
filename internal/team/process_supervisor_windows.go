//go:build windows

package team

import "context"

// windowsProcessSupervisor is an explicit stub: Windows needs a Job Object
// implementation before reliable process-tree cleanup can be guaranteed
// (§16.2). Every method fails closed with ErrProcessSupervisorUnsupported
// rather than silently offering weaker guarantees than Unix.
type windowsProcessSupervisor struct{}

func newPlatformProcessSupervisor() ProcessSupervisor { return windowsProcessSupervisor{} }

func (windowsProcessSupervisor) Start(context.Context, ProcessSpec) (ProcessHandle, error) {
	return nil, ErrProcessSupervisorUnsupported
}

func (windowsProcessSupervisor) Interrupt(context.Context, ProcessHandle) error {
	return ErrProcessSupervisorUnsupported
}

func (windowsProcessSupervisor) TerminateTree(context.Context, ProcessHandle) error {
	return ErrProcessSupervisorUnsupported
}

func (windowsProcessSupervisor) KillTree(context.Context, ProcessHandle) error {
	return ErrProcessSupervisorUnsupported
}
