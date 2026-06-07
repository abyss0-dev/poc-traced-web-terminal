package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// qemuProcess wraps a launched QEMU guest and implements Process.
type qemuProcess struct {
	cmd *exec.Cmd
}

// stopGrace bounds how long Stop waits for a graceful SIGTERM exit before
// escalating to SIGKILL.
const stopGrace = 5 * time.Second

// KVMAvailable reports whether the process can use KVM acceleration. It is the
// authoritative check (group membership and device permissions both matter), so
// it opens /dev/kvm rather than merely stat-ing it. Callers use it to warn when
// the launch will fall back to slow TCG emulation.
func KVMAvailable() bool { return kvmAvailable() }

// kvmAvailable is the internal implementation behind KVMAvailable and the
// per-launch accelerator decision.
func kvmAvailable() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// qemuArgs builds the QEMU command line for one target. KVM acceleration with
// host CPU passthrough is mandatory; callers must verify KVMAvailable first.
func qemuArgs(tc TargetConfig) []string {
	mem := tc.Memory
	if mem == "" {
		mem = "1024"
	}
	hostfwd := fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", tc.SSHPort)

	return []string{
		"-enable-kvm", "-machine", "accel=kvm", "-cpu", "host",
		"-m", mem,
		"-drive", "file=" + tc.Image + ",if=virtio",
		"-drive", "file=" + tc.Seed + ",if=virtio,format=raw",
		"-netdev", hostfwd,
		"-device", "virtio-net-pci,netdev=net0",
		"-display", "none",
		"-serial", "none",
		"-monitor", "none",
	}
}

// launchQEMU starts one Ubuntu guest under QEMU user-mode networking with a
// host-loopback port forward to the guest's sshd. KVM acceleration is required.
func launchQEMU(tc TargetConfig) (Process, error) {
	cmd := exec.Command("qemu-system-x86_64", qemuArgs(tc)...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	// Own the process in its own group so Stop terminates the whole guest.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch qemu for %q: %w", tc.ID, err)
	}
	return &qemuProcess{cmd: cmd}, nil
}

// Stop terminates the guest, first with SIGTERM and then, after a grace period,
// with SIGKILL.
func (p *qemuProcess) Stop() error {
	if p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()

	select {
	case <-done:
		return nil
	case <-time.After(stopGrace):
		_ = p.cmd.Process.Kill()
		<-done
		return nil
	}
}
