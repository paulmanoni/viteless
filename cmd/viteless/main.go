// Command viteless is a thin CLI around the viteless library so example
// apps can be run without a host framework: `viteless dev <dir>` starts the
// HMR dev server, `viteless build <dir>` writes a production bundle.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/paulmanoni/viteless"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	// Pull the positional <dir> out first so flags work in any position
	// (Go's flag package otherwise stops parsing at the first non-flag).
	dir, flags := splitDirArg(os.Args[2:])
	switch os.Args[1] {
	case "dev":
		fs := flag.NewFlagSet("dev", flag.ExitOnError)
		addr := fs.String("addr", "127.0.0.1:5173", "listen address")
		proxy := fs.String("proxy", "", "backend origin to proxy unmatched requests to (e.g. http://127.0.0.1:8080)")
		_ = fs.Parse(flags)
		if dir == "" {
			usage()
		}
		runDev(dir, *addr, *proxy)
	case "build":
		fs := flag.NewFlagSet("build", flag.ExitOnError)
		out := fs.String("out", "", "output dir (default <dir>/dist)")
		_ = fs.Parse(flags)
		if dir == "" {
			usage()
		}
		runBuild(dir, *out)
	default:
		usage()
	}
}

// splitDirArg returns the first bare (non-flag) argument as the directory
// and the remaining args as flags, so `dev <dir> -addr x` and
// `dev -addr x <dir>` both work.
func splitDirArg(args []string) (dir string, flags []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			dir = a
			flags = append(flags, args[:i]...)
			flags = append(flags, args[i+1:]...)
			return dir, flags
		}
	}
	return "", args
}

func usage() {
	fmt.Fprintln(os.Stderr, `viteless — zero-Node Vite-for-Go

usage:
  viteless dev   <dir> [-addr host:port] [-proxy origin]
  viteless build <dir> [-out dir]

examples:
  viteless dev   ./examples/vue-app
  viteless dev   ./examples/react-app
  viteless build ./examples/vue-app`)
	os.Exit(2)
}

func logf(format string, a ...any) { fmt.Printf(format+"\n", a...) }

func runDev(root, addr, proxy string) {
	d, err := viteless.Dev(viteless.DevConfig{
		Root:        root,
		Addr:        addr,
		ProxyTarget: proxy,
		Logf:        logf,
	})
	if err != nil {
		log.Fatalf("viteless dev: %v", err)
	}
	defer d.Close()
	fmt.Printf("\n  ➜  viteless dev   %s\n  ➜  serving         %s\n  ➜  press Ctrl-C to stop\n\n", d.URL(), root)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := d.Wait(ctx); err != nil {
		log.Fatalf("viteless dev: %v", err)
	}
}

func runBuild(root, out string) {
	res, err := viteless.Build(viteless.BuildConfig{
		Root:   root,
		OutDir: out,
		Logf:   logf,
	})
	if err != nil {
		log.Fatalf("viteless build: %v", err)
	}
	if len(res.Warnings) > 0 {
		for _, w := range res.Warnings {
			fmt.Println("warning:", w)
		}
	}
	if len(res.Errors) > 0 {
		for _, e := range res.Errors {
			fmt.Fprintln(os.Stderr, "error:", e)
		}
		os.Exit(1)
	}
	fmt.Printf("built → %s (%d files)\n", res.OutDir, len(res.OutputFiles))
}
