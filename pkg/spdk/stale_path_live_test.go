//go:build stale_path_integration

package spdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/longhorn/go-spdk-helper/pkg/initiator"

	commontypes "github.com/longhorn/go-common-libs/types"
	helperutil "github.com/longhorn/go-spdk-helper/pkg/util"

	lhtypes "github.com/longhorn/longhorn-spdk-engine/pkg/types"
)

// This CI-only fixture uses two loopback Linux NVMe/TCP listeners for one
// disposable file-backed namespace. It exercises real native multipath and
// cleanup with fio running, but substitutes kernel nvmet for SPDK target RPCs.
type liveCleanupFixture struct {
	t            *testing.T
	volume       string
	nqn          string
	namespace    string
	subsystemDir string
	namespaceDir string
	portDirs     []string
	links        []string
	ports        []string
	dir          string
}

func liveCommand(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func liveWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func liveEventually(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func newLiveCleanupFixture(t *testing.T) *liveCleanupFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("live NVMe/TCP validation requires root on a disposable runner")
	}
	multipath, err := os.ReadFile("/sys/module/nvme_core/parameters/multipath")
	if err != nil || strings.TrimSpace(string(multipath)) != "Y" {
		t.Fatalf("native NVMe multipath is not enabled: %q, %v", multipath, err)
	}
	f := &liveCleanupFixture{
		t:      t,
		volume: fmt.Sprintf("stale-path-ci-%d-%d", os.Getpid(), time.Now().UnixNano()),
		dir:    t.TempDir(),
	}
	f.nqn = getStableVolumeNQN(f.volume)
	f.subsystemDir = filepath.Join("/sys/kernel/config/nvmet/subsystems", f.nqn)
	if err := os.Mkdir(f.subsystemDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.cleanup)
	liveWrite(t, filepath.Join(f.subsystemDir, "attr_allow_any_host"), "1")

	backingPath := filepath.Join(f.dir, "backing.img")
	backing, err := os.OpenFile(backingPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.Truncate(64 << 20); err != nil {
		_ = backing.Close()
		t.Fatal(err)
	}
	if err := backing.Close(); err != nil {
		t.Fatal(err)
	}
	f.namespaceDir = filepath.Join(f.subsystemDir, "namespaces", "1")
	if err := os.Mkdir(f.namespaceDir, 0755); err != nil {
		t.Fatal(err)
	}
	liveWrite(t, filepath.Join(f.namespaceDir, "device_path"), backingPath)
	liveWrite(t, filepath.Join(f.namespaceDir, "device_nguid"), getStableVolumeNGUID(f.volume))
	liveWrite(t, filepath.Join(f.namespaceDir, "enable"), "1")

	for i := 0; i < 2; i++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		portDir := filepath.Join("/sys/kernel/config/nvmet/ports", port)
		if err := os.Mkdir(portDir, 0755); err != nil {
			t.Fatal(err)
		}
		f.portDirs = append(f.portDirs, portDir)
		f.ports = append(f.ports, port)
		liveWrite(t, filepath.Join(portDir, "addr_trtype"), "tcp")
		liveWrite(t, filepath.Join(portDir, "addr_adrfam"), "ipv4")
		liveWrite(t, filepath.Join(portDir, "addr_traddr"), "127.0.0.1")
		liveWrite(t, filepath.Join(portDir, "addr_trsvcid"), port)
		ana := "optimized"
		if i == 1 {
			ana = "inaccessible"
		}
		liveWrite(t, filepath.Join(portDir, "ana_groups", "1", "ana_state"), ana)
		link := filepath.Join(portDir, "subsystems", f.nqn)
		if err := os.Symlink(f.subsystemDir, link); err != nil {
			t.Fatal(err)
		}
		f.links = append(f.links, link)
	}
	f.connect(0)

	executor, err := helperutil.NewExecutor(commontypes.ProcDirectory)
	if err != nil {
		t.Fatal(err)
	}
	liveEventually(t, "initial namespace", func() bool {
		devices, err := initiator.GetDevices("127.0.0.1", f.ports[0], f.nqn, executor)
		if err != nil || len(devices) != 1 || len(devices[0].Namespaces) != 1 {
			return false
		}
		f.namespace = devices[0].Namespaces[0].NameSpace
		return f.namespace != ""
	})
	t.Logf("namespace=%s NQN=%s ports=%v", f.namespace, f.nqn, f.ports)
	return f
}

func (f *liveCleanupFixture) connect(index int) {
	liveCommand(f.t, "nvme", "connect", "-t", "tcp", "-a", "127.0.0.1", "-s", f.ports[index],
		"-n", f.nqn, "--ctrl-loss-tmo=5", "--reconnect-delay=1")
}

func (f *liveCleanupFixture) cleanup() {
	// Only this fixture's unique NQN, links, namespace, and ports are removed.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "nvme", "disconnect", "-n", f.nqn).CombinedOutput()
	if err != nil {
		f.t.Logf("fixture disconnect: %v: %s", err, output)
	}
	for _, link := range f.links {
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			f.t.Errorf("remove fixture link %s: %v", link, err)
		}
	}
	for _, portDir := range f.portDirs {
		if err := os.Remove(portDir); err != nil {
			f.t.Errorf("remove fixture port %s: %v", portDir, err)
		}
	}
	if f.namespaceDir != "" {
		if err := os.WriteFile(filepath.Join(f.namespaceDir, "enable"), []byte("0"), 0600); err != nil {
			f.t.Errorf("disable fixture namespace: %v", err)
		}
		if err := os.Remove(f.namespaceDir); err != nil {
			f.t.Errorf("remove fixture namespace: %v", err)
		}
	}
	if err := os.Remove(f.subsystemDir); err != nil {
		f.t.Errorf("remove fixture subsystem: %v", err)
	}
}

func (f *liveCleanupFixture) controllerPorts() map[string]string {
	f.t.Helper()
	executor, err := helperutil.NewExecutor(commontypes.ProcDirectory)
	if err != nil {
		f.t.Fatal(err)
	}
	subsystems, err := initiator.GetSubsystems(executor)
	if err != nil {
		f.t.Fatal(err)
	}
	ports := map[string]string{}
	for _, subsystem := range subsystems {
		if subsystem.NQN != f.nqn {
			continue
		}
		for _, path := range subsystem.Paths {
			ip, port := initiator.GetIPAndPortFromControllerAddress(path.Address)
			if helperutil.IsSameNvmeAddr(ip, "127.0.0.1") {
				ports[port] = path.State
			}
		}
	}
	return ports
}

func liveIOBytes(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "write_bytes:") {
			var value uint64
			_, _ = fmt.Sscanf(line, "write_bytes: %d", &value)
			return value
		}
	}
	return 0
}

func TestStalePathLiveSwitchover(t *testing.T) {
	for _, phased := range []bool{false, true} {
		for _, failCleanup := range []bool{false, true} {
			t.Run(fmt.Sprintf("phased=%v/cleanup-error=%v", phased, failCleanup), func(t *testing.T) {
				f := newLiveCleanupFixture(t)
				devicePath := "/dev/" + f.namespace
				ef := NewEngineFrontend("ef-live", "engine-0", f.volume, lhtypes.FrontendSPDKTCPBlockdev,
					64<<20, 0, 0, make(chan interface{}, 16), nil)
				ef.State = lhtypes.InstanceStateRunning
				ef.NvmeTcpFrontend.TargetIP = "127.0.0.1"
				initialPort, err := strconv.Atoi(f.ports[0])
				if err != nil {
					t.Fatal(err)
				}
				ef.NvmeTcpFrontend.TargetPort = int32(initialPort)
				ef.NvmeTcpFrontend.Nqn = f.nqn
				ef.NvmeTcpFrontend.Nguid = getStableVolumeNGUID(f.volume)
				ef.Endpoint = devicePath
				ef.syncCurrentNVMeTCPPathLocked()
				ef.metadataDir = filepath.Join(f.dir, "metadata")
				ef.initiator = &initiator.Initiator{Endpoint: devicePath, NVMeTCPInfo: &initiator.NVMeTCPInfo{
					SubsystemNQN: f.nqn, NamespaceName: f.namespace,
				}}
				ef.getInitiatorEndpointFn = func() string { return devicePath }
				ef.loadInitiatorEndpointFn = func(bool) error { return nil }
				ef.loadInitiatorNVMeDeviceInfoFn = func(string, string, string) error { return nil }
				ef.connectNvmeTCPPathFn = func(address, port string) error {
					index := 0
					if port == f.ports[1] {
						index = 1
					}
					f.connect(index)
					return nil
				}
				ef.waitForNvmeTCPControllerLiveFn = func(address string, port int32) error {
					liveEventually(t, "new controller live", func() bool {
						return f.controllerPorts()[strconv.Itoa(int(port))] == initiator.NvmeControllerStateLive
					})
					return nil
				}
				ef.setRemoteEngineTargetANAStateFn = func(address, engine string, state NvmeTCPANAState) error {
					index := 0
					if engine == "engine-1" {
						index = 1
					}
					return os.WriteFile(filepath.Join(f.portDirs[index], "ana_groups", "1", "ana_state"), []byte(state), 0600)
				}
				ef.syncRemoteEngineTargetANAStatesFn = func(oldIP, oldEngine, newIP, newEngine string) error {
					for _, step := range []struct {
						engine string
						state  NvmeTCPANAState
					}{
						{newEngine, NvmeTCPANAStateNonOptimized},
						{oldEngine, NvmeTCPANAStateInaccessible},
						{newEngine, NvmeTCPANAStateOptimized},
					} {
						if err := ef.setRemoteEngineTargetANAState("127.0.0.1", step.engine, step.state); err != nil {
							return err
						}
					}
					return nil
				}
				if failCleanup {
					ef.disconnectStaleNvmeTCPPathFn = func(string, string, string) error {
						return fmt.Errorf("injected targeted disconnect failure")
					}
				}

				fioOutput := filepath.Join(f.dir, "fio.json")
				fioCtx, fioCancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer fioCancel()
				fio := exec.CommandContext(fioCtx, "/usr/bin/fio",
					"--name=stale-path-live", "--filename="+devicePath, "--rw=write",
					"--ioengine=libaio", "--iodepth=8", "--bs=4k", "--direct=1",
					"--verify=crc32c", "--verify_fatal=1", "--verify_backlog=128",
					"--verify_backlog_batch=128", "--verify_state_save=0",
					"--time_based=1", "--runtime=25", "--size=32m", "--thread=1",
					"--group_reporting=1", "--output-format=json", "--output="+fioOutput)
				fio.Stdout = os.Stdout
				fio.Stderr = os.Stderr
				if err := fio.Start(); err != nil {
					t.Fatal(err)
				}
				fioDone := false
				t.Cleanup(func() {
					if !fioDone {
						_ = fio.Process.Kill()
						_ = fio.Wait()
					}
				})
				liveEventually(t, "verified I/O before switchover", func() bool { return liveIOBytes(fio.Process.Pid) > 1<<20 })
				handoffs := 3
				if failCleanup {
					handoffs = 1
				}
				current := 0
				for handoff := 0; handoff < handoffs; handoff++ {
					next := 1 - current
					bytesBefore := liveIOBytes(fio.Process.Pid)
					for _, phase := range blockdevCleanupTestPhases(phased) {
						if err := ef.SwitchOverTarget(nil, fmt.Sprintf("engine-%d", next), "127.0.0.1:"+f.ports[next], phase); err != nil {
							t.Fatal(err)
						}
					}
					snapshot := liveCommand(t, "nvme", "list-subsys", "-o", "json", devicePath)
					t.Logf("handoff %d host state: %s", handoff+1, snapshot)
					ready, err := nvmeTCPPathIsOptimized(snapshot, f.nqn, "127.0.0.1", f.ports[next])
					if err != nil || !ready {
						t.Fatalf("replacement not ready: %v, %v", ready, err)
					}
					if failCleanup {
						if len(f.controllerPorts()) != 2 {
							t.Fatal("failed cleanup should leave the old controller for timeout fallback")
						}
						// Stop only the superseded listener. Observe the retained
						// controller reconnecting, then expiring through the
						// fixture's five-second ctrl-loss-tmo while I/O continues.
						if err := os.Remove(f.links[current]); err != nil {
							t.Fatal(err)
						}
						liveEventually(t, "old controller reconnecting", func() bool {
							return f.controllerPorts()[f.ports[current]] == "connecting"
						})
						t.Log("observed old controller reconnecting after injected disconnect failure")
						liveEventually(t, "ctrl-loss-tmo fallback removes old controller", func() bool {
							ports := f.controllerPorts()
							return len(ports) == 1 && ports[f.ports[next]] == initiator.NvmeControllerStateLive
						})
						t.Log("ctrl-loss-tmo fallback removed only the old controller")
					} else {
						liveEventually(t, "only replacement controller remains", func() bool {
							ports := f.controllerPorts()
							return len(ports) == 1 && ports[f.ports[next]] == initiator.NvmeControllerStateLive
						})
					}
					liveEventually(t, "I/O progress after switchover", func() bool { return liveIOBytes(fio.Process.Pid) > bytesBefore+(1<<20) })
					current = next
				}
				err = fio.Wait()
				fioDone = true
				report, readErr := os.ReadFile(fioOutput)
				t.Logf("fio report: %s", report)
				if err != nil || readErr != nil {
					t.Fatalf("fio failed: %v, report error: %v", err, readErr)
				}
				var results struct{ Jobs []struct{ Error int } }
				if err := json.Unmarshal(report, &results); err != nil || len(results.Jobs) != 1 || results.Jobs[0].Error != 0 {
					t.Fatalf("fio verification failed: %v, %+v", err, results)
				}
			})
		}
	}
}
