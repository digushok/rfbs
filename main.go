package main

// Ozon rFBS — автоматизация остатков и статусов заказов.
// Запускает локальный веб-интерфейс и открывает его в браузере.

import (
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Версию подставляет сборка из файла VERSION: -ldflags "-X main.appVersion=..."
var appVersion = "dev"

const repoURL = "https://github.com/digushok/rfbs"

//go:embed static
var staticFS embed.FS

func main() {
	dir := dataDir()
	logf, _ := os.OpenFile(filepath.Join(dir, "logs", "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	appLog := func(format string, a ...any) {
		if logf != nil {
			fmt.Fprintf(logf, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
		}
	}

	// Если программа уже запущена — просто открываем её окно.
	var ln net.Listener
	var url string
	for port := 8766; port < 8776; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		l, err := net.Listen("tcp", addr)
		if err == nil {
			ln, url = l, "http://"+addr+"/"
			break
		}
		if isOurApp("http://" + addr + "/api/ping") {
			openBrowser("http://" + addr + "/")
			return
		}
	}
	if ln == nil {
		appLog("не удалось открыть порт")
		os.Exit(1)
	}

	store := LoadStore(dir)
	mocks := NewMockRegistry()
	mgr := NewManager(dir, store, mocks)
	srv := NewServer(dir, store, mgr)

	// Возобновляем автоматизацию магазинов, где она была включена.
	_, list := store.Snapshot()
	for _, p := range list {
		if p.Running {
			if err := mgr.Start(p.ID); err != nil {
				mgr.Log(p.ID).Add("err", "Не удалось возобновить автоматизацию: %v", err)
			} else {
				mgr.Log(p.ID).Add("info", "Автоматизация возобновлена после перезапуска программы.")
			}
		}
	}

	sub, _ := fs.Sub(staticFS, "static")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	srv.Routes(mux)
	appLog("запуск %s, данные: %s, адрес: %s", appVersion, dir, url)
	if os.Getenv("OZON_RFBS_NO_BROWSER") == "" {
		go func() {
			time.Sleep(300 * time.Millisecond)
			openBrowser(url)
		}()
	}
	if err := http.Serve(ln, mux); err != nil {
		appLog("сервер остановлен: %v", err)
	}
}

func isOurApp(url string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	r, err := c.Get(url)
	if err != nil {
		return false
	}
	r.Body.Close()
	return r.Header.Get("X-App") == "ozon-rfbs"
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

func openFolder(path string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("explorer", path).Start()
	case "darwin":
		exec.Command("open", path).Start()
	default:
		exec.Command("xdg-open", path).Start()
	}
}
