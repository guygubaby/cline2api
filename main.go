//go:build !desktop

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"
)

func main() {
	defer flushRuntimeState()
	loginMode := flag.Bool("login", false, "Run OAuth device login flow and add account to pool")
	captureMode := flag.Bool("capture", false, "Run interactive OAuth capture (records ALL traffic)")
	port := flag.Int("port", 3457, "Proxy server port")
	host := flag.String("host", configuredHost(), "Proxy server listen host (0.0.0.0 = all interfaces)")
	addAccount := flag.Bool("add-account", false, "Add a new account via OAuth to the pool")
	showList := flag.Bool("list", false, "List all accounts in the pool")
	startMode := flag.Bool("start", false, "Build, start proxy, and open admin panel in browser")
	flag.Parse()

	if *startMode {
		buildAndStart(*host, *port)
		return
	}

	if *captureMode {
		if err := doFullCapture(); err != nil {
			log.Fatalf("Capture failed: %v", err)
		}
		return
	}

	if *loginMode || *addAccount {
		acc, err := addAccountFromDeviceAuth()
		if err != nil {
			log.Fatalf("Login failed: %v", err)
		}
		fmt.Printf("Account added to pool successfully!\n")
		fmt.Printf("  Account ID: %s\n", acc.AccountID)
		fmt.Printf("  Email:      %s\n", acc.Email)
		fmt.Printf("  Status:     %s\n", acc.Status)
		fmt.Println("\nRun without flags to start the proxy with account rotation.")
		return
	}

	if *showList {
		accounts := listAccounts()
		if len(accounts) == 0 {
			fmt.Println("No accounts in pool. Use --add-account to add one.")
			return
		}
		fmt.Printf("\n=== Account Pool (%d accounts) ===\n\n", len(accounts))
		for i, a := range accounts {
			fmt.Printf("  %d. [%s] %s (status: %s, used: %d)\n",
				i+1, a.AccountID, a.Email, a.Status, a.UsageCount)
		}
		fmt.Println()
		return
	}

	if err := startProxy(*host, *port); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Proxy failed: %v", err)
		os.Exit(1)
	}
	// 监听被 restartListener 接管（重启后原 ListenAndServe 返回 ErrServerClosed），主 goroutine 保持阻塞
	select {}
}

// configuredHost 返回监听地址：环境变量 > 后台保存的 listenHost > 默认 127.0.0.1。
func configuredHost() string {
	if v := os.Getenv("CLINE_PROXY_HOST"); v != "" {
		return v
	}
	if v := loadPool().ListenHost; v != "" {
		return v
	}
	return "127.0.0.1"
}

func buildAndStart(host string, port int) {
	exe := "cline-proxy.exe"
	if runtime.GOOS != "windows" {
		exe = "./cline-proxy"
	}

	fmt.Println("Building proxy...")
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("Build failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Build complete.")

	running := false
	if runtime.GOOS == "windows" {
		out, _ := exec.Command("powershell", "-Command",
			"Get-Process cline-proxy -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Id").Output()
		if len(out) > 0 {
			running = true
		}
	}

	if running {
		fmt.Println("Proxy is already running.")
	} else {
		fmt.Println("Starting proxy...")
		startCmd := exec.Command(exe)
		startCmd.Stdout = os.Stdout
		startCmd.Stderr = os.Stderr
		if err := startCmd.Start(); err != nil {
			fmt.Printf("Start failed: %v\n", err)
			os.Exit(1)
		}
		time.Sleep(2 * time.Second)
		fmt.Println("Proxy started.")
	}

	url := fmt.Sprintf("http://%s:%d/admin/", effectiveAdminHost(host), port)
	fmt.Printf("\nAdmin panel: %s\n", url)

	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}
