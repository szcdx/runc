//go:build cgo && seccomp
// +build cgo,seccomp

package seccomp

import (
	"errors"
	"fmt"
	"os"

	libseccomp "github.com/seccomp/libseccomp-golang"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/szcdx/runc/libcontainer/configs"
	"github.com/szcdx/runc/libcontainer/seccomp/patchbpf"
)

var (
	actTrace = libseccomp.ActTrace.SetReturnCode(int16(unix.EPERM))
	actErrno = libseccomp.ActErrno.SetReturnCode(int16(unix.EPERM))
)

const (
	// Linux system calls can have at most 6 arguments
	syscallMaxArguments int = 6
)

// InitSeccomp installs the seccomp filters to be used in the container as
// specified in config. Returns the seccomp file descriptor if any of the
// filters include a SCMP_ACT_NOTIFY action.
func InitSeccomp(config *configs.Seccomp) (*os.File, error) {
	if config == nil {
		return nil, errors.New("cannot initialize Seccomp - nil config passed")
	}

	defaultAction, err := getAction(config.DefaultAction, config.DefaultErrnoRet)
	if err != nil {
		return nil, errors.New("error initializing seccomp - invalid default action")
	}

	// Ignore the error since pre-2.4 libseccomp is treated as API level 0.
	apiLevel, _ := libseccomp.GetAPI()
	for _, call := range config.Syscalls {
		if call.Action == configs.Notify {
			if apiLevel < 6 {
				return nil, fmt.Errorf("seccomp notify unsupported: API level: got %d, want at least 6. Please try with libseccomp >= 2.5.0 and Linux >= 5.7", apiLevel)
			}

			// We can't allow the write syscall to notify to the seccomp agent.
			// After InitSeccomp() is called, we need to syncParentSeccomp() to write the seccomp fd plain
			// number, so the parent sends it to the seccomp agent. If we use SCMP_ACT_NOTIFY on write, we
			// never can write the seccomp fd to the parent and therefore the seccomp agent never receives
			// the seccomp fd and runc is hang during initialization.
			//
			// Note that read()/close(), that are also used in syncParentSeccomp(), _can_ use SCMP_ACT_NOTIFY.
			// Because we write the seccomp fd on the pipe to the parent, the parent is able to proceed and
			// send the seccomp fd to the agent (it is another process and not subject to the seccomp
			// filter). We will be blocked on read()/close() inside syncParentSeccomp() but if the seccomp
			// agent allows those syscalls to proceed, initialization works just fine and the agent can
			// handle future read()/close() syscalls as it wanted.
			if call.Name == "write" {
				return nil, errors.New("SCMP_ACT_NOTIFY cannot be used for the write syscall")
			}
		}
	}

	// See comment on why write is not allowed. The same reason applies, as this can mean handling write too.
	if defaultAction == libseccomp.ActNotify {
		return nil, errors.New("SCMP_ACT_NOTIFY cannot be used as default action")
	}

	filter, err := libseccomp.NewFilter(defaultAction)
	if err != nil {
		return nil, fmt.Errorf("error creating filter: %w", err)
	}

	// Add extra architectures
	for _, arch := range config.Architectures {
		scmpArch, err := libseccomp.GetArchFromString(arch)
		if err != nil {
			return nil, fmt.Errorf("error validating Seccomp architecture: %w", err)
		}
		if err := filter.AddArch(scmpArch); err != nil {
			return nil, fmt.Errorf("error adding architecture to seccomp filter: %w", err)
		}
	}

	// Add extra flags.
	for _, flag := range config.Flags {
		if err := setFlag(filter, flag); err != nil {
			return nil, err
		}
	}

	// Enable libseccomp binary tree optimization for longer rulesets.
	//
	// The number below chosen semi-arbitrarily, considering the following:
	// 1. libseccomp <= 2.5.4 misbehaves when binary tree optimization
	// is enabled and there are 0 rules.
	// 2. All known libseccomp versions (2.5.0 to 2.5.4) generate a binary
	// tree with 4 syscalls per node.
	if len(config.Syscalls) > 32 {
		if err := filter.SetOptimize(2); err != nil {
			// The error is not fatal and is probably means we have older libseccomp.
			logrus.Debugf("seccomp binary tree optimization not available: %v", err)
		}
	}

	// Unset no new privs bit
	if err := filter.SetNoNewPrivsBit(false); err != nil {
		return nil, fmt.Errorf("error setting no new privileges: %w", err)
	}

	// Add a rule for each syscall
	for _, call := range config.Syscalls {
		if call == nil {
			return nil, errors.New("encountered nil syscall while initializing Seccomp")
		}

		if err := matchCall(filter, call, defaultAction); err != nil {
			return nil, err
		}
	}

	seccompFd, err := patchbpf.PatchAndLoad(config, filter)
	if err != nil {
		return nil, fmt.Errorf("error loading seccomp filter into kernel: %w", err)
	}
	return seccompFd, nil
}

type unknownFlagError struct {
	flag specs.LinuxSeccompFlag
}

func (e *unknownFlagError) Error() string {
	return "seccomp flag " + string(e.flag) + " is not known to runc"
}

func setFlag(filter *libseccomp.ScmpFilter, flag specs.LinuxSeccompFlag) error {
	switch flag {
	case flagTsync:
		// libseccomp-golang always use filterAttrTsync when
		// possible so all goroutines will receive the same
		// rules, so there is nothing to do. It does not make
		// sense to apply the seccomp filter on only one
		// thread; other threads will be terminated after exec
		// anyway.
		return nil
	case specs.LinuxSeccompFlagLog:
		if err := filter.SetLogBit(true); err != nil {
			return fmt.Errorf("error adding log flag to seccomp filter: %w", err)
		}
		return nil
	case specs.LinuxSeccompFlagSpecAllow:
		if err := filter.SetSSB(true); err != nil {
			return fmt.Errorf("error adding SSB flag to seccomp filter: %w", err)
		}
		return nil
	}
	// NOTE when adding more flags above, do not forget to also:
	// - add new flags to `flags` slice in config.go;
	// - add new flag values to flags_value() in tests/integration/seccomp.bats;
	// - modify func filterFlags in patchbpf/ accordingly.

	return &unknownFlagError{flag: flag}
}

// FlagSupported checks if the flag is known to runc and supported by
// currently used libseccomp and kernel (i.e. it can be set).
func FlagSupported(flag specs.LinuxSeccompFlag) error {
	filter := &libseccomp.ScmpFilter{}
	err := setFlag(filter, flag)

	// For flags we don't know, setFlag returns unknownFlagError.
	var uf *unknownFlagError
	if errors.As(err, &uf) {
		return err
	}
	// For flags that are known to runc and libseccomp-golang but can not
	// be applied because either libseccomp or the kernel is too old,
	// seccomp.VersionError is returned.
	var verErr *libseccomp.VersionError
	if errors.As(err, &verErr) {
		// Not supported by libseccomp or the kernel.
		return err
	}

	// All other flags are known and supported.
	return nil
}

// Convert Libcontainer Action to Libseccomp ScmpAction
func getAction(act configs.Action, errnoRet *uint) (libseccomp.ScmpAction, error) {
	switch act {
	case configs.Kill, configs.KillThread:
		return libseccomp.ActKillThread, nil
	case configs.Errno:
		if errnoRet != nil {
			return libseccomp.ActErrno.SetReturnCode(int16(*errnoRet)), nil
		}
		return actErrno, nil
	case configs.Trap:
		return libseccomp.ActTrap, nil
	case configs.Allow:
		return libseccomp.ActAllow, nil
	case configs.Trace:
		if errnoRet != nil {
			return libseccomp.ActTrace.SetReturnCode(int16(*errnoRet)), nil
		}
		return actTrace, nil
	case configs.Log:
		return libseccomp.ActLog, nil
	case configs.Notify:
		return libseccomp.ActNotify, nil
	case configs.KillProcess:
		return libseccomp.ActKillProcess, nil
	default:
		return libseccomp.ActInvalid, errors.New("invalid action, cannot use in rule")
	}
}

// Convert Libcontainer Operator to Libseccomp ScmpCompareOp
func getOperator(op configs.Operator) (libseccomp.ScmpCompareOp, error) {
	switch op {
	case configs.EqualTo:
		return libseccomp.CompareEqual, nil
	case configs.NotEqualTo:
		return libseccomp.CompareNotEqual, nil
	case configs.GreaterThan:
		return libseccomp.CompareGreater, nil
	case configs.GreaterThanOrEqualTo:
		return libseccomp.CompareGreaterEqual, nil
	case configs.LessThan:
		return libseccomp.CompareLess, nil
	case configs.LessThanOrEqualTo:
		return libseccomp.CompareLessOrEqual, nil
	case configs.MaskEqualTo:
		return libseccomp.CompareMaskedEqual, nil
	default:
		return libseccomp.CompareInvalid, errors.New("invalid operator, cannot use in rule")
	}
}

// Convert Libcontainer Arg to Libseccomp ScmpCondition
func getCondition(arg *configs.Arg) (libseccomp.ScmpCondition, error) {
	cond := libseccomp.ScmpCondition{}

	if arg == nil {
		return cond, errors.New("cannot convert nil to syscall condition")
	}

	op, err := getOperator(arg.Op)
	if err != nil {
		return cond, err
	}

	return libseccomp.MakeCondition(arg.Index, op, arg.Value, arg.ValueTwo)
}

// Add a rule to match a single syscall
func matchCall(filter *libseccomp.ScmpFilter, call *configs.Syscall, defAct libseccomp.ScmpAction) error {
	if call == nil || filter == nil {
		return errors.New("cannot use nil as syscall to block")
	}

	if len(call.Name) == 0 {
		return errors.New("empty string is not a valid syscall")
	}

	// Convert the call's action to the libseccomp equivalent
	callAct, err := getAction(call.Action, call.ErrnoRet)
	if err != nil {
		return fmt.Errorf("action in seccomp profile is invalid: %w", err)
	}
	if callAct == defAct {
		// This rule is redundant, silently skip it
		// to avoid error from AddRule.
		return nil
	}

	// If we can't resolve the syscall, assume it is not supported
	// by this kernel. Warn about it, don't error out.
	callNum, err := libseccomp.GetSyscallFromName(call.Name)
	if err != nil {
		logrus.Debugf("unknown seccomp syscall %q ignored", call.Name)
		return nil
	}

	// Unconditional match - just add the rule
	if len(call.Args) == 0 {
		if err := filter.AddRule(callNum, callAct); err != nil {
			return fmt.Errorf("error adding seccomp filter rule for syscall %s: %w", call.Name, err)
		}
	} else {
		// If two or more arguments have the same condition,
		// Revert to old behavior, adding each condition as a separate rule
		argCounts := make([]uint, syscallMaxArguments)
		conditions := []libseccomp.ScmpCondition{}

		for _, cond := range call.Args {
			newCond, err := getCondition(cond)
			if err != nil {
				return fmt.Errorf("error creating seccomp syscall condition for syscall %s: %w", call.Name, err)
			}

			argCounts[cond.Index] += 1

			conditions = append(conditions, newCond)
		}

		hasMultipleArgs := false
		for _, count := range argCounts {
			if count > 1 {
				hasMultipleArgs = true
				break
			}
		}

		if hasMultipleArgs {
			// Revert to old behavior
			// Add each condition attached to a separate rule
			for _, cond := range conditions {
				condArr := []libseccomp.ScmpCondition{cond}

				if err := filter.AddRuleConditional(callNum, callAct, condArr); err != nil {
					return fmt.Errorf("error adding seccomp rule for syscall %s: %w", call.Name, err)
				}
			}
		} else {
			// No conditions share same argument
			// Use new, proper behavior
			if err := filter.AddRuleConditional(callNum, callAct, conditions); err != nil {
				return fmt.Errorf("error adding seccomp rule for syscall %s: %w", call.Name, err)
			}
		}
	}

	return nil
}

// Version returns major, minor, and micro.
func Version() (uint, uint, uint) {
	return libseccomp.GetLibraryVersion()
}

// Enabled is true if seccomp support is compiled in.
const Enabled = true
