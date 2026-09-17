package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"bilibili-txt/internal/config"
	"bilibili-txt/internal/webui"
)

var uiContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

var startWebUI = func(ctx context.Context, cfg *config.Config) error {
	srv, err := webui.New(webui.Config{
		HTTPPort:       cfg.Server.Port,
		Cfg:            cfg,
		OpenBrowser:    cfg.Server.OpenBrowser,
		QuitWhenClosed: cfg.Server.OpenBrowser,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "界面已启动：http://%s/ （关闭窗口或 Ctrl-C 退出）\n", srv.Addr())
	return srv.Run(ctx)
}

func runUI(stderr io.Writer, configPath string) error {
	base, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg := config.Merge(base, &config.Config{})
	cfg.Finalize(stderr)
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := uiContext()
	defer stop()
	return startWebUI(ctx, cfg)
}

var errUIIncompatibleFlags = errors.New("--force-asr、--keep-intermediate、--overwrite、--skip、--output-dir 只能与视频链接一起使用")

func rejectUIIncompatibleFlags(c *cobra.Command) error {
	for _, name := range []string{"force-asr", "keep-intermediate", "overwrite", "skip", "output-dir"} {
		if c.Flags().Changed(name) {
			return errUIIncompatibleFlags
		}
	}
	return nil
}
