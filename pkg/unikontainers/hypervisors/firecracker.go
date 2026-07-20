// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hypervisors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

const (
	FirecrackerVmm    VmmType = "firecracker"
	FirecrackerBinary string  = "firecracker"
	FCJsonFilename    string  = "fc.json"
)

type Firecracker struct {
	binaryPath string
	binary     string
}

type FirecrackerBootSource struct {
	ImagePath  string `json:"kernel_image_path"`
	BootArgs   string `json:"boot_args"`
	InitrdPath string `json:"initrd_path,omitempty"`
}

type FirecrackerMachine struct {
	VcpuCount       uint   `json:"vcpu_count"`
	MemSizeMiB      uint64 `json:"mem_size_mib"`
	Smt             bool   `json:"smt"`
	TrackDirtyPages bool   `json:"track_dirty_pages"`
}

type FirecrackerDrive struct {
	DriveID   string `json:"drive_id"`
	IsRO      bool   `json:"is_read_only"`
	IsRootDev bool   `json:"is_root_device"`
	HostPath  string `json:"path_on_host"`
}

type FirecrackerNet struct {
	IfaceID  string `json:"iface_id"`
	GuestMAC string `json:"guest_mac,omitempty"`
	HostIF   string `json:"host_dev_name"`
}

type FirecrackerVSockDev struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
	VSockID  string `json:"vsock_id"`
}

type FirecrackerConfig struct {
	Source  FirecrackerBootSource `json:"boot-source"`
	Machine FirecrackerMachine    `json:"machine-config"`
	Drives  []FirecrackerDrive    `json:"drives"`
	NetIfs  []FirecrackerNet      `json:"network-interfaces,omitempty"`
	VSock   FirecrackerVSockDev   `json:"vsock,omitempty"`
}

func (fc *Firecracker) Signal(pid int, signal unix.Signal) error {
	return unix.Kill(pid, signal)
}

func (fc *Firecracker) Stop(pid int) error {
	return killProcess(pid)
}

func (fc *Firecracker) Ok() error {
	return nil
}

func (fc *Firecracker) UsesKVM() bool {
	return true
}

// SupportsSharedfs returns a bool value depending on the monitor support for shared-fs
func (fc *Firecracker) SupportsSharedfs(_ string) bool {
	return false
}

func (fc *Firecracker) Path() string {
	return fc.binaryPath
}

func (fc *Firecracker) BuildExecCmd(args types.ExecArgs, ukernel types.Unikernel) ([]string, error) {
	// FIXME: Note for getting unikernel specific options.
	// Due to the way FC operates, we have not encountered any guest specific
	// options yet. However, we need to revisit how we can use guest specific
	// options in FC, since the string return value of the Monitor related
	// functions in the unikernel interface do not integrate well with FC's
	// json configuration.
	apiSockPath := args.SocketPath
	if apiSockPath == "" {
		apiSockPath = filepath.Join("/tmp/", args.ContainerID+".sock")
	}
	cmdString := fc.Path() + " --api-sock " + apiSockPath
	JSONConfigFile := filepath.Join("/tmp/", FCJsonFilename)
	if args.BootMode == "config-file" {
		// config-file-based: Firecracker boots itself from the JSON config file
		// below; the socket stays open only for use after the guest is running.
		cmdString += " --config-file " + JSONConfigFile
	}
	if !args.Seccomp {
		cmdString += " --no-seccomp"
	}

	FCConfig := buildFirecrackerConfig(args, ukernel)
	FCConfigJSON, err := json.Marshal(FCConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Firecracker config: %w", err)
	}
	if err = os.WriteFile(JSONConfigFile, FCConfigJSON, 0o644); err != nil { //nolint: gosec
		return nil, fmt.Errorf("failed to save Firecracker json config: %w", err)
	}
	vmmLog.WithField("Json", string(FCConfigJSON)).Debug("Firecracker json config")

	exArgs := strings.Split(cmdString, " ")
	return exArgs, nil
}

// PreExec performs pre-execution setup. Firecracker has no special pre-exec requirements.
func (fc *Firecracker) PreExec(_ types.ExecArgs) error {
	return nil
}

// buildFirecrackerConfig builds the microVM configuration from args and
// ukernel. Used both to write the JSON config file (config-file-based boot)
// and to drive the same configuration over the API socket (API-based boot),
// so both paths always agree on what the guest actually gets configured
// with.
func buildFirecrackerConfig(args types.ExecArgs, ukernel types.Unikernel) *FirecrackerConfig {
	// VM config for Firecracker
	fcMem := DefaultMemory
	if args.MemSizeB != 0 {
		fcMem = bytesToMiB(args.MemSizeB)
		// Check if memory is too small
		if fcMem == 0 {
			fcMem = DefaultMemory
		}
	}
	// NOTE: Firecracker supports only one initrd.
	// Therefore, we depend on the guest/unikernel implementation
	// to properly handle that case and concatenate the initrd
	// files if there are more than one. Hence, always give priority
	// to the initrd taken from args.
	extraMonArgs := ukernel.MonitorCli()
	initrdPath := args.InitrdPath
	if initrdPath == "" {
		initrdPath = extraMonArgs.ExtraInitrd
	}
	FCMachine := FirecrackerMachine{
		VcpuCount:       args.VCPUs,
		MemSizeMiB:      fcMem,
		Smt:             false,
		TrackDirtyPages: false,
	}

	// Net config for Firecracker
	FCNet := make([]FirecrackerNet, 0)
	if args.Net.TapDev != "" {
		AnIF := FirecrackerNet{
			IfaceID:  "net1",
			GuestMAC: args.Net.MAC,
			HostIF:   args.Net.TapDev,
		}
		FCNet = append(FCNet, AnIF)
	}

	// Block config for Firecracker
	// TODO: Add support for block devices in FIrecracker
	FCDrives := make([]FirecrackerDrive, 0)

	bArgs := ukernel.MonitorBlockCli()
	for _, blockArg := range bArgs {
		aBlock := FirecrackerDrive{
			DriveID:   blockArg.ID,
			IsRO:      false,
			IsRootDev: false,
			HostPath:  blockArg.Path,
		}
		if blockArg.ID == "rootfs" {
			aBlock.IsRootDev = true
		}
		FCDrives = append(FCDrives, aBlock)
	}
	FCSource := FirecrackerBootSource{
		ImagePath:  args.UnikernelPath,
		BootArgs:   args.Command,
		InitrdPath: initrdPath,
	}

	var FCVSockDev FirecrackerVSockDev
	if args.VAccelType == "vsock" {
		FCVSockDev = FirecrackerVSockDev{
			GuestCID: args.VSockDevID,
			UDSPath:  args.VSockDevPath + "/vaccel.sock",
			VSockID:  "root",
		}
	}

	return &FirecrackerConfig{
		Source:  FCSource,
		Machine: FCMachine,
		Drives:  FCDrives,
		NetIfs:  FCNet,
		VSock:   FCVSockDev,
	}
}

// FirecrackerSession is a Firecracker child process being configured over its
// API socket in stages, while the caller performs its own setup work between
// the stages. Create it with SpawnSocketVMM, feed it configuration with the
// Configure* methods as each piece becomes available, boot the guest with
// StartGuest, then hand the calling process over with Supervise.
type FirecrackerSession struct {
	cmd    *exec.Cmd
	client *firecrackerClient
}

// SpawnSocketVMM starts Firecracker as a child process with only its API
// socket enabled, and establishes the single persistent connection all later
// configuration stages use (it survives a later changeRoot; see
// firecrackerClient).
//
// This is called early in Exec, before the monitor rootfs is prepared and
// before changeRoot, so unlike the exec path the child keeps the caller's
// current (pre-pivot) mount view for its lifetime. It does inherit the
// sandbox's network namespace, which Exec has already joined. When uid/gid
// are non-zero the child is started directly under that credential, since
// the caller only drops its own privileges (setupUser) much later.
func (fc *Firecracker) SpawnSocketVMM(args types.ExecArgs, uid, gid uint32) (*FirecrackerSession, error) {
	socketPath := args.SocketPath
	if socketPath == "" {
		socketPath = filepath.Join("/tmp/", args.ContainerID+".sock")
	}
	execCmd := []string{fc.Path(), "--api-sock", socketPath}
	if !args.Seccomp {
		execCmd = append(execCmd, "--no-seccomp")
	}

	cmd := exec.Command(execCmd[0], execCmd[1:]...) //nolint: gosec
	cmd.Env = args.Environment
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if uid != 0 || gid != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uid, Gid: gid},
		}
	}
	vmmLog.WithField("command", execCmd).Debug("starting Firecracker as a supervised child")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start firecracker: %w", err)
	}

	client := newFirecrackerClient(socketPath)
	if err := client.connect(5 * time.Second); err != nil {
		s := &FirecrackerSession{cmd: cmd, client: client}
		s.Kill()
		return nil, fmt.Errorf("firecracker socket never became ready: %w", err)
	}
	return &FirecrackerSession{cmd: cmd, client: client}, nil
}

// ConfigureMachine sends the vCPU/memory configuration. Nothing but the
// container spec is needed for this, so it is the first stage, sent right
// after the spawn.
func (s *FirecrackerSession) ConfigureMachine(ctx context.Context, args types.ExecArgs) error {
	fcMem := DefaultMemory
	if args.MemSizeB != 0 {
		fcMem = bytesToMiB(args.MemSizeB)
		if fcMem == 0 {
			fcMem = DefaultMemory
		}
	}
	machine := FirecrackerMachine{
		VcpuCount:       args.VCPUs,
		MemSizeMiB:      fcMem,
		Smt:             false,
		TrackDirtyPages: false,
	}
	vmmLog.Debug("staged boot: sending machine-config")
	return s.client.putMachineConfig(ctx, machine)
}

// ConfigureNetwork attaches the container's network interface. Firecracker
// opens the tap device during this call, so it must only run once the
// network setup that creates the tap has finished. No-op without a tap.
func (s *FirecrackerSession) ConfigureNetwork(ctx context.Context, net types.NetDevParams) error {
	if net.TapDev == "" {
		return nil
	}
	iface := FirecrackerNet{
		IfaceID:  "net1",
		GuestMAC: net.MAC,
		HostIF:   net.TapDev,
	}
	vmmLog.Debug("staged boot: sending network-interfaces")
	return s.client.putNetworkIface(ctx, iface)
}

// ConfigureGuest sends everything that depends on unikernel.Init having run:
// the block devices (their IDs/paths come from the unikernel's
// MonitorBlockCli), the boot source (its boot args are the unikernel command
// line, which bakes in the resolved network), and the vsock device if any.
//
// The child was spawned before changeRoot, so it resolves paths in the
// pre-pivot mount view; monRootfs (the directory the caller will pivot into)
// is therefore prefixed onto every path the child has to open.
func (s *FirecrackerSession) ConfigureGuest(ctx context.Context, args types.ExecArgs, ukernel types.Unikernel, monRootfs string) error {
	cfg := buildFirecrackerConfig(args, ukernel)

	cfg.Source.ImagePath = filepath.Join(monRootfs, cfg.Source.ImagePath)
	if cfg.Source.InitrdPath != "" {
		cfg.Source.InitrdPath = filepath.Join(monRootfs, cfg.Source.InitrdPath)
	}
	for i := range cfg.Drives {
		cfg.Drives[i].HostPath = filepath.Join(monRootfs, cfg.Drives[i].HostPath)
	}
	if cfg.VSock.UDSPath != "" {
		cfg.VSock.UDSPath = filepath.Join(monRootfs, cfg.VSock.UDSPath)
	}

	vmmLog.Debug("staged boot: sending drives, boot-source and vsock")
	if err := s.client.putDrives(ctx, cfg.Drives); err != nil {
		return err
	}
	if err := s.client.putBootSource(ctx, cfg.Source); err != nil {
		return err
	}
	return s.client.putVSock(ctx, cfg.VSock)
}

// StartGuest powers on the configured microVM.
func (s *FirecrackerSession) StartGuest(ctx context.Context) error {
	vmmLog.Debug("staged boot: sending InstanceStart")
	return s.client.startGuest(ctx)
}

// Kill terminates the child and reaps it. For error paths before Supervise.
func (s *FirecrackerSession) Kill() {
	_ = s.cmd.Process.Kill()
	_, _ = s.cmd.Process.Wait()
}

// Supervise hands the calling process over to the child for the rest of its
// life: it forwards SIGTERM/SIGINT and, once the child exits, exits this
// process with the child's exit code, mirroring the semantics syscall.Exec
// would have had. The caller must not exit before the child, since it is
// the container's init process.
//
// On success this function does not return: it calls os.Exit with the
// child's exit status once the child exits.
func (s *FirecrackerSession) Supervise() error {
	// Forward the signals containerd would send to stop the container.
	// SIGKILL cannot be caught, so it is not listed here: if it arrives,
	// this process dies immediately and the child is left running, a known
	// gap for this bounded experiment, not yet handled.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig, ok := <-sigCh
		if !ok {
			return
		}
		if sg, ok := sig.(syscall.Signal); ok {
			_ = s.cmd.Process.Signal(sg)
		}
	}()

	waitErr := s.cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			vmmLog.WithError(waitErr).Error("firecracker exited with an unexpected error")
			exitCode = 1
		}
	}
	os.Exit(exitCode)
	return nil // unreachable
}
