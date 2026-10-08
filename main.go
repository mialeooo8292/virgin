package main

import (
	"context"
	"errors"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// BERMUDA Stealth Gateway NG — Master Edge Entrypoint & Runtime Orchestrator
// Dynamic cgroup v1/v2 Budgeting, 5-Stage Zero-Loss Drain, Go 1.24 Baseline
// Automated Cloudflare Edge Tuning & 0-RTT Connection Resumption Engine
// Invariant: Zero External Dependencies, Kernel-Tuned TCP Sockets, Leak-Free
// ---------------------------------------------------------------------------

const (
	defaultPort            = "8080"
	fallbackSelfMemMB      = 128
	fallbackXrayMemMB      = 550
	defaultGOMAXPROCS      = 2
	gatewayMemPercent      = 12
	xrayMemPercent         = 55
	httpDrainTimeout       = 10 * time.Second
	edgeKeepAlivePeriod    = 15 * time.Second
	drainPropagationWindow = 500 * time.Millisecond

	// Linux-specific TCP socket option (0x12)
	tcpUserTimeoutOpt = 18
)

// cgroupMemoryLimit reads cgroup v2 memory.max, falling back to v1 memory.limit_in_bytes.
func cgroupMemoryLimit() (uint64, bool) {
	candidates := []string{
		"/sys/fs/cgroup/memory.max",                  // cgroup v2
		"/sys/fs/cgroup/memory/memory.limit_in_bytes", // cgroup v1
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" || s == "max" {
			continue
		}
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil || n == 0 || n >= 1<<50 { // Ignore unlimited thresholds (~9.22e18)
			continue
		}
		return n, true
	}
	return 0, false
}

// deriveMemoryBudget clamps allocations to a safe 67% ceiling of available container memory.
// Reserves 33% headroom for kernel TCP socket buffers, Linux page cache, and stacks.
func deriveMemoryBudget() (gwMB, xrayMB int) {
	gwMB, xrayMB = fallbackSelfMemMB, fallbackXrayMemMB
	limit, limited := cgroupMemoryLimit()
	if limited {
		totalMB := int(limit >> 20)
		gwMB = totalMB * gatewayMemPercent / 100
		xrayMB = totalMB * xrayMemPercent / 100

		// Enforce safety clamp: sum must not exceed 67% of container capacity
		allowedMB := totalMB * 67 / 100
		if gwMB < 32 {
			gwMB = 32
		}
		if xrayMB < 64 {
			xrayMB = 64
		}
		if total := gwMB + xrayMB; total > allowedMB && allowedMB >= 96 {
			gwMB = allowedMB * gatewayMemPercent / 67
			xrayMB = allowedMB * xrayMemPercent / 67
		}
	}

	gwMB = getEnvInt("BERMUDA_SELF_MEM_MB", gwMB)
	xrayMB = getEnvInt("BERMUDA_XRAY_MEM_MB", xrayMB)
	return gwMB, xrayMB
}

// applyMemoryCeiling configures Go runtime soft memory limit (GOMEMLIMIT) and GC percentage.
func applyMemoryCeiling(selfMB int) {
	debug.SetMemoryLimit(int64(selfMB) << 20)
	debug.SetGCPercent(100)
}

// cgroupCPUQuota auto-detects CPU limits from cgroup v2 or v1 hierarchies.
func cgroupCPUQuota() (float64, bool) {
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(string(data))
		if len(f) == 2 && f[0] != "max" {
			q, e1 := strconv.ParseFloat(f[0], 64)
			p, e2 := strconv.ParseFloat(f[1], 64)
			if e1 == nil && e2 == nil && q > 0 && p > 0 {
				return q / p, true
			}
		}
		return 0, false
	}
	qb, e1 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	pb, e2 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if e1 == nil && e2 == nil {
		q, e3 := strconv.ParseFloat(strings.TrimSpace(string(qb)), 64)
		p, e4 := strconv.ParseFloat(strings.TrimSpace(string(pb)), 64)
		if e3 == nil && e4 == nil && q > 0 && p > 0 {
			return q / p, true
		}
	}
	return 0, false
}

// applyGOMAXPROCS dynamically pins the Go scheduler to container CFS quotas.
func applyGOMAXPROCS() int {
	n := getEnvInt("BERMUDA_GOMAXPROCS", 0)
	if n == 0 {
		if q, ok := cgroupCPUQuota(); ok {
			n = int(math.Ceil(q))
		}
	}
	if n <= 0 {
		n = defaultGOMAXPROCS
	}
	if cpus := runtime.NumCPU(); n > cpus {
		n = cpus
	}
	runtime.GOMAXPROCS(n)
	return n
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// teardown executes an ordered, 5-stage graceful drain sequence within Railway's 25s budget.
func teardown(srv *http.Server, gw *Gateway, sup *Supervisor, supCancel context.CancelFunc, graceful bool) {
	// Railway provides 25s draining interval. We use 23s hard deadline for safety buffer.
	hardDeadline := time.Now().Add(23 * time.Second)

	gw.SetDraining()
	if graceful {
		log.Println("[Gateway] Stage 1/5: Health status flipped to 503 (traffic shedding)...")
		time.Sleep(drainPropagationWindow)
	}

	if graceful {
		log.Println("[Gateway] Stage 2/5: Draining HTTP server listeners and short-lived requests...")
		httpDeadline := minTime(time.Now().Add(5*time.Second), hardDeadline)
		httpCtx, cancelHTTP := context.WithDeadline(context.Background(), httpDeadline)
		if err := srv.Shutdown(httpCtx); err != nil {
			log.Printf("[Gateway] HTTP server drain timeout (%v); forcing listener closure", err)
			_ = srv.Close()
		} else {
			log.Println("[Gateway] HTTP server listener drained successfully")
		}
		cancelHTTP()

		log.Println("[Gateway] Stage 3/5: Waiting for active WebSocket/hijacked tunnels to conclude...")
		tunnelDeadline := minTime(hardDeadline.Add(-8*time.Second), time.Now().Add(httpDrainTimeout))
		if tunnelDeadline.After(time.Now()) {
			tunnelCtx, cancelTunnel := context.WithDeadline(context.Background(), tunnelDeadline)
			gw.WaitTunnels(tunnelCtx)
			cancelTunnel()
		}
	} else {
		_ = srv.Close()
	}

	log.Println("[Gateway] Stage 4/5: Force-closing remaining hijacked sockets and backend pools...")
	gw.CloseTunnels()
	gw.CloseIdleBackendConns()

	log.Println("[Gateway] Stage 5/5: Tearing down child Xray process group...")
	supCancel()
	remainingSupTime := time.Until(hardDeadline)
	if remainingSupTime <= 0 {
		remainingSupTime = 2 * time.Second
	}
	sup.Stop(remainingSupTime)
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Println("[Gateway] Initializing BERMUDA Stealth Gateway NG...")

	// 1. Calculate dynamic memory ceilings and scheduler quotas from cgroup hierarchy
	gwMB, xrayMB := deriveMemoryBudget()
	applyMemoryCeiling(gwMB)
	procs := applyGOMAXPROCS()

	// Export derived parameters so supervisor propagates them to the child daemon
	_ = os.Setenv("BERMUDA_XRAY_MEM_MB", strconv.Itoa(xrayMB))
	_ = os.Setenv("BERMUDA_XRAY_GOMAXPROCS", strconv.Itoa(procs))
	log.Printf("[Runtime] Dynamic memory ceiling: Gateway=%dMiB, Xray=%dMiB (GOGC=100) | GOMAXPROCS=%d",
		gwMB, xrayMB, procs)

	port := getEnv("PORT", defaultPort)

	// 2. Instantiate supervisor and run preflight syntax validation
	sup := NewSupervisor()
	if err := sup.Preflight(); err != nil {
		log.Printf("[Gateway] Warning: Supervisor preflight issue: %v. Continuing to start...", err)
	}

	// 3. Instantiate reverse proxy edge engine
	gw := NewGateway(sup)

	// 4. Automated Cloudflare Edge Tuning & Idempotent Ruleset Orchestrator
	// Dispatched asynchronously in background: guarantees zero delay during Railway PaaS cold boots.
	cfToken := strings.TrimSpace(getEnv("CLOUDFLARE_API_TOKEN", os.Getenv("BERMUDA_CF_API_TOKEN")))
	cfZoneID := strings.TrimSpace(getEnv("CLOUDFLARE_ZONE_ID", os.Getenv("BERMUDA_CF_ZONE_ID")))
	if cfToken != "" && cfZoneID != "" {
		cf := NewCloudflareClient(cfToken, cfZoneID)
		go cf.Tune(context.Background())
	} else {
		log.Println("[Cloudflare] Notice: CLOUDFLARE_API_TOKEN or CLOUDFLARE_ZONE_ID unset. Edge auto-tuning bypassed.")
	}

	// 5. Decoupled Context Architecture:
	// sigCtx handles termination signals from Railway PaaS / Docker.
	// supCtx governs the Xray child supervisor independently.
	sigCtx, stopSig := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSig()
	supCtx, supCancel := context.WithCancel(context.Background())

	// 6. Run supervisor loop in a dedicated background goroutine
	supErrCh := make(chan error, 1)
	go func() {
		supErrCh <- sup.Run(supCtx)
	}()

	// 7. Configure HTTP edge server with line-rate socket tuning
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,  // Protects against Slowloris header trickle attacks
		ReadTimeout:       30 * time.Second,  // Bounded request header/body read
		WriteTimeout:      0,                 // Must be 0 for long-lived full-duplex streams (XHTTP, WS)
		IdleTimeout:       16 * time.Minute,  // 960s: safely exceeds Cloudflare 900s origin reuse limit
		MaxHeaderBytes:    32 << 10,          // 32 KiB header limit
		ConnState:         gw.TrackConnState,
	}

	// 8. Bind TCP listener with modern KeepAliveConfig and TCP_USER_TIMEOUT
	lc := net.ListenConfig{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     edgeKeepAlivePeriod,
			Interval: edgeKeepAlivePeriod,
			Count:    4,
		},
		Control: func(network, address string, c syscall.RawConn) error {
			var controlErr error
			err := c.Control(func(fd uintptr) {
				// Set TCP_USER_TIMEOUT (90s). Accepted child sockets inherit this on Linux.
				if e := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeoutOpt, 90000); e != nil {
					controlErr = e
				}
			})
			if err != nil {
				return err
			}
			return controlErr
		},
	}

	ln, err := lc.Listen(context.Background(), "tcp", srv.Addr)
	if err != nil {
		log.Fatalf("[Gateway] Fatal: Cannot bind listener on %s: %v", srv.Addr, err)
	}

	serverErrCh := make(chan error, 1)
	go func() {
		log.Printf("[Gateway] Edge listener active on :%s (PID %d, GOMAXPROCS=%d)", port, os.Getpid(), procs)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
	}()

	// 9. Await termination signals or fatal process errors
	select {
	case err := <-serverErrCh:
		log.Printf("[Gateway] Fatal: HTTP server failure: %v", err)
		teardown(srv, gw, sup, supCancel, false)
		os.Exit(1)
	case err := <-supErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[Gateway] Fatal: Supervisor halted unexpectedly: %v", err)
		}
		teardown(srv, gw, sup, supCancel, false)
		os.Exit(1)
	case <-sigCtx.Done():
		log.Println("[Gateway] Termination signal intercepted. Commencing graceful teardown...")
	}

	// 10. Execute 5-stage graceful drain sequence
	teardown(srv, gw, sup, supCancel, true)
	log.Println("[Gateway] BERMUDA Stealth Gateway shutdown complete. Ports released cleanly. Exit 0.")
}
