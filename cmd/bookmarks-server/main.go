// SPDX-License-Identifier: AGPL-3.0-or-later

// Command bookmarks-server is the sync server for the Bookmarks reader.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/vagramchic/bookmarks-server/internal/certs"
	"github.com/vagramchic/bookmarks-server/internal/pairing"
	"github.com/vagramchic/bookmarks-server/internal/server"
	"github.com/vagramchic/bookmarks-server/internal/store"
)

const usage = `bookmarks-server — сервер синхронизации читалки Bookmarks

Команды:
  serve       [--data DIR] [--listen :8443] [--name "Дом"] [--max-file-size 500M] [--max-devices N]
  pair        [--data DIR] [--ttl 10m] [--max-devices N]
  devices     [--data DIR]
  remove      [--data DIR] DEVICE_ID
  fingerprint [--data DIR]

Каталог данных по умолчанию: $BOOKMARKS_DATA, иначе
$XDG_DATA_HOME/bookmarks-server (~/.local/share/bookmarks-server).
Ограничение числа устройств по умолчанию: $BOOKMARKS_MAX_DEVICES, иначе нет.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "ошибка:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}
	cmd, args := args[0], args[1:]
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	data := flags.String("data", defaultDataDir(), "каталог данных")
	switch cmd {
	case "serve":
		listen := flags.String("listen", ":8443", "адрес и порт")
		name := flags.String("name", "", "имя сервера (по умолчанию из сертификата или имя хоста)")
		maxSize := flags.String("max-file-size", "500M", "наибольший размер файла книги")
		maxDevices := maxDevicesFlag(flags)
		if err := flags.Parse(args); err != nil {
			return err
		}
		size, err := parseSize(*maxSize)
		if err != nil {
			return fmt.Errorf("--max-file-size: %w", err)
		}
		if *maxDevices < 0 {
			return errors.New("--max-devices не может быть отрицательным")
		}
		return serve(ctx, *data, *listen, *name, size, *maxDevices, stdout, stderr)
	case "pair":
		ttl := flags.Duration("ttl", 10*time.Minute, "сколько действует код")
		maxDevices := maxDevicesFlag(flags)
		if err := flags.Parse(args); err != nil {
			return err
		}
		return pair(ctx, *data, *ttl, *maxDevices, stdout)
	case "devices":
		if err := flags.Parse(args); err != nil {
			return err
		}
		return devices(ctx, *data, stdout)
	case "remove":
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("укажите id устройства: bookmarks-server remove DEVICE_ID")
		}
		return remove(ctx, *data, flags.Arg(0), stdout)
	case "fingerprint":
		if err := flags.Parse(args); err != nil {
			return err
		}
		fp, err := certs.LoadFingerprint(*data)
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("сертификата ещё нет: сначала запустите bookmarks-server serve")
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, certs.Group(fp))
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("неизвестная команда %q", cmd)
	}
}

// maxDevicesFlag defines --max-devices, defaulting to $BOOKMARKS_MAX_DEVICES
// so that serve and pair see the same limit from one environment.
func maxDevicesFlag(flags *flag.FlagSet) *int {
	def := 0
	if v := os.Getenv("BOOKMARKS_MAX_DEVICES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			def = n
		}
	}
	return flags.Int("max-devices", def, "наибольшее число сопряжённых устройств, 0 — без ограничения ($BOOKMARKS_MAX_DEVICES)")
}

func defaultDataDir() string {
	if d := os.Getenv("BOOKMARKS_DATA"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "bookmarks-server")
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "bookmarks-server")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "bookmarks-server-data"
	}
	return filepath.Join(home, ".local", "share", "bookmarks-server")
}

// prepareDataDir creates the data directory with mode 700.
func prepareDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

func openStore(dir string) (*store.Store, error) {
	if err := prepareDataDir(dir); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dir, store.DBFile))
}

// parseSize accepts bytes or a number with a K, M or G suffix (powers of 1024).
func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	mul := int64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mul = 1 << 10
	case strings.HasSuffix(s, "M"):
		mul = 1 << 20
	case strings.HasSuffix(s, "G"):
		mul = 1 << 30
	}
	if mul > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("неверный размер %q", s)
	}
	return n * mul, nil
}

func serve(ctx context.Context, dir, listen, name string, maxSize int64, maxDevices int, stdout, stderr io.Writer) error {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	st, err := openStore(dir)
	if err != nil {
		return err
	}
	defer st.Close()

	cn := name
	if cn == "" {
		if cn, err = os.Hostname(); err != nil || cn == "" {
			cn = "Bookmarks"
		}
	}
	cert, created, err := certs.LoadOrCreate(dir, cn)
	if err != nil {
		return err
	}
	if name == "" {
		name = cert.Leaf.Subject.CommonName
	}
	fp := certs.Fingerprint(cert.Certificate[0])
	if created {
		fmt.Fprintf(stdout, "Создан новый сертификат сервера в %s.\n", dir)
		fmt.Fprintln(stdout, "Не теряйте server.key: без него все устройства придётся сопрягать заново.")
	}
	fmt.Fprintf(stdout, "Сервер «%s», отпечаток сертификата:\n  %s\n", name, certs.Group(fp))
	fmt.Fprintln(stdout, "Чтобы подключить устройство, выполните: bookmarks-server pair")

	filesDir := filepath.Join(dir, "files")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return err
	}
	h := server.New(server.Config{
		Store:             st,
		FilesDir:          filesDir,
		Name:              name,
		ServerFingerprint: fp,
		MaxFileSize:       maxSize,
		MaxDevices:        maxDevices,
		Logger:            log,
	})
	srv := server.HTTPServer(listen, h, server.TLSConfig(cert), log)

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", listen)
		errc <- srv.ListenAndServeTLS("", "")
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func pair(ctx context.Context, dir string, ttl time.Duration, maxDevices int, stdout io.Writer) error {
	if ttl <= 0 {
		return errors.New("--ttl должен быть больше нуля")
	}
	fp, err := certs.LoadFingerprint(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("сертификата ещё нет: сначала запустите bookmarks-server serve")
	}
	if err != nil {
		return err
	}
	st, err := openStore(dir)
	if err != nil {
		return err
	}
	defer st.Close()
	if maxDevices > 0 {
		n, err := st.CountDevices(ctx)
		if err != nil {
			return err
		}
		if n >= maxDevices {
			return fmt.Errorf("уже сопряжено %d из %d разрешённых устройств: отключите лишнее (bookmarks-server devices, remove) или увеличьте --max-devices", n, maxDevices)
		}
	}
	code, err := pairing.NewCode()
	if err != nil {
		return err
	}
	expires := time.Now().Add(ttl)
	if err := st.SetPairingCode(ctx, code, expires); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Код сопряжения:  %s\n", pairing.Format(code))
	fmt.Fprintf(stdout, "Действует до:    %s\n", expires.Format("15:04:05"))
	fmt.Fprintf(stdout, "Отпечаток:       %s\n", certs.Group(fp))
	if addrs := localAddresses(); len(addrs) > 0 {
		fmt.Fprintln(stdout, "Адреса сервера (порт тот, что у serve, по умолчанию 8443):")
		for _, a := range addrs {
			fmt.Fprintf(stdout, "  %s\n", a)
		}
	}
	fmt.Fprintln(stdout, "\nВведите код в приложении: Синхронизация → Сопрячь.")
	fmt.Fprintln(stdout, "Сверьте отпечаток с тем, что покажет приложение.")
	return nil
}

// localAddresses lists non-loopback unicast addresses of this machine.
func localAddresses() []string {
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range ifaddrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipn.IP.String())
	}
	return out
}

func devices(ctx context.Context, dir string, stdout io.Writer) error {
	st, err := openStore(dir)
	if err != nil {
		return err
	}
	defer st.Close()
	devs, err := st.Devices(ctx)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Fprintln(stdout, "Сопряжённых устройств нет. Подключить: bookmarks-server pair")
		return nil
	}
	const layout = "2006-01-02 15:04"
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tИМЯ\tПЛАТФОРМА\tОТПЕЧАТОК\tСОПРЯЖЕНО\tБЫЛО В СЕТИ")
	for _, d := range devs {
		seen := "никогда"
		if d.LastSeenAt != nil {
			seen = d.LastSeenAt.Local().Format(layout)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s…\t%s\t%s\n", d.ID, d.Name, d.Platform,
			certs.Group(d.Fingerprint[:16]), d.PairedAt.Local().Format(layout), seen)
	}
	return tw.Flush()
}

func remove(ctx context.Context, dir, id string, stdout io.Writer) error {
	st, err := openStore(dir)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.RemoveDevice(ctx, id); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("устройства %s нет; список: bookmarks-server devices", id)
	} else if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Устройство %s отключено. Его записи и файлы остались на сервере.\n", id)
	return nil
}
