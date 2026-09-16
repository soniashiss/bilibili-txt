package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"bilibili-txt/internal/biliurl"
	"bilibili-txt/internal/config"
	"bilibili-txt/internal/logger"
	"bilibili-txt/internal/naming"
	"bilibili-txt/internal/pathx"
	"bilibili-txt/internal/pipeline"
	"bilibili-txt/internal/preflight"
	"bilibili-txt/internal/wire"
)

type NormalizedInput struct {
	URL  string
	BVID string
}

func newRootCmd() *cobra.Command {
	var (
		outputDir        string
		forceASR         bool
		keepIntermediate bool
		overwrite        bool
		skip             bool
		noInteractive    bool
		configPath       string
	)

	cmd := &cobra.Command{
		Use:   "bilibili-txt [视频链接]",
		Short: "从 Bilibili 视频链接生成文字稿（优先字幕，其次 ASR）；不带参数时启动图形界面",
		Long: `bilibili-txt 优先使用视频的官方 CC 字幕生成文字稿；若无字幕则退回音频 + whisper-cli ASR。

不带任何参数直接运行 bilibili-txt 时，会在本机启动图形界面（浏览器窗口），
在页面里粘贴视频链接即可转换，历史文稿也能在界面中查看。

支持的输入格式：
  - https://www.bilibili.com/video/BVxxx[/...]
  - https://b23.tv/xxx（短链）
  - 纯 BVxxx（透传给 yt-dlp）

Shell 引号提示（zsh 用户尤其注意）：
  Bilibili 分享的完整链接常带 ?spm_id_from=... 或 &vd_source=... 之类的追踪参数。
  未加引号时 zsh 会把 ? * [ 等字符当作 glob 展开，报 "zsh: no matches found"，
  参数根本不会送到 bilibili-txt。反引号 ` + "`" + ` 会被 zsh 当作命令替换，同样危险。
  建议永远给 URL 加单引号，例如：
    bilibili-txt 'https://www.bilibili.com/video/BVxxx/?spm_id_from=...'
  或者直接去掉 ? 后面的追踪参数、或只传 BVxxx。`,
		Example: `  # 不带参数：启动图形界面
  bilibili-txt

  # 单引号包住完整 URL（推荐，兼容 URL 里的 ? & 反引号 等）
  bilibili-txt 'https://www.bilibili.com/video/BV1xx411c7mD/?spm_id_from=333.1007'

  # 去掉追踪参数后可以不加引号
  bilibili-txt https://www.bilibili.com/video/BV1xx411c7mD/

  # 纯 BVID 也行
  bilibili-txt BV1xx411c7mD

  # 强制 ASR 分支
  bilibili-txt --force-asr BV1xx411c7mD

  # 指定输出目录
  bilibili-txt -o ~/Documents/transcripts BV1xx411c7mD`,
		Version: version,
		Args: func(c *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("最多接受 1 个视频链接，收到 %d 个", len(args))
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				if err := rejectUIIncompatibleFlags(c); err != nil {
					return err
				}
				return runUI(c.ErrOrStderr(), configPath)
			}

			if overwrite && skip {
				return errors.New("--overwrite 与 --skip 不能同时使用")
			}

			input, err := normalizeInput(args[0])
			if err != nil {
				return err
			}

			overlay := &config.Config{}
			if outputDir != "" {
				abs, err := normalizePathFlag(outputDir, "--output-dir")
				if err != nil {
					return err
				}
				overlay.OutputDir = abs
			}
			switch {
			case overwrite:
				overlay.Naming.OnConflict = naming.StrategyOverwrite
			case skip:
				overlay.Naming.OnConflict = naming.StrategySkip
			}

			base, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			cfg := config.Merge(base, overlay)
			cfg.Finalize(c.ErrOrStderr())
			if err := cfg.Validate(); err != nil {
				return err
			}

			loggerOpts := logger.Options{
				Level:          cfg.Logging.Level,
				WriteDebugFile: cfg.Debug,
				Format:         cfg.Logging.Format,
				File:           cfg.Logging.File,
				DebugLogFile:   cfg.Logging.DebugFile,
				Bvid:           input.BVID,
			}
			if err := logger.Init(loggerOpts); err != nil {
				fmt.Fprintf(c.ErrOrStderr(), "bilibili-txt: init logger (falling back to default): %s\n", err)
			}
			defer func() { _ = logger.Default().Close() }()

			if !cfg.SkipPreflight {
				results := preflight.Run(cfg)
				if !preflight.AllOK(results) {
					fmt.Fprintln(c.ErrOrStderr(), "preflight failed:")
					fmt.Fprint(c.ErrOrStderr(), preflight.FormatFailures(results))
					return errPreflightFailed
				}
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			in := pipeline.Input{
				URL:              input.URL,
				ForceASR:         forceASR,
				KeepIntermediate: keepIntermediate,
				IsTTY:            defaultIsTTY(),
				NoInteractive:    noInteractive,
				Prompter:         &naming.StdinPrompter{In: os.Stdin, Out: c.ErrOrStderr()},
			}
			pdeps := wire.BuildDeps(cfg)

			result, err := pipeline.Run(ctx, in, cfg, pdeps)
			if err != nil {
				return err
			}
			printSuccess(c.OutOrStdout(), result)
			return nil
		},
	}

	cmd.Flags().StringVarP(&outputDir, "output-dir", "o", "", "输出目录（默认 ./transcripts）")
	cmd.Flags().BoolVar(&forceASR, "force-asr", false, "跳过字幕探测，强制走 ASR")
	cmd.Flags().BoolVar(&keepIntermediate, "keep-intermediate", false, "保留中间产物（音频、原始 srt）")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "输出已存在时直接覆盖")
	cmd.Flags().BoolVar(&skip, "skip", false, "输出已存在时直接跳过")
	cmd.Flags().BoolVar(&noInteractive, "no-interactive", false, "非交互模式（冲突时直接失败）")
	cmd.Flags().StringVar(&configPath, "config", "", "配置文件路径（默认 ./config/config.yaml；仅测试/调试用）")
	_ = cmd.Flags().MarkHidden("config")

	cmd.Flags().BoolP("help", "h", false, "查看帮助")
	cmd.Flags().BoolP("version", "v", false, "查看版本")

	return cmd
}

var errPreflightFailed = errors.New("preflight failed")

func printSuccess(stdout io.Writer, r *pipeline.Result) {
	if r == nil {
		return
	}
	title := ""
	videoDur := "unknown"
	if r.Metadata != nil {
		title = r.Metadata.Title
		if r.Metadata.Duration > 0 {
			videoDur = r.Metadata.Duration.Round(time.Second).String()
		}
	}
	fmt.Fprintf(stdout, "bilibili-txt: %s -> %s (took=%s, video=%s, title=%q)\n",
		r.Source, r.OutputPath, r.Duration.Round(time.Millisecond), videoDur, title)
}

func defaultIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

var argvBackslashEscapeRe = regexp.MustCompile(`\\([?&=#;,:/@+$!*()\[\]{}~'"` + "`" + `])`)

func sanitizeArgvURL(raw string) string {
	s := strings.TrimSpace(raw)
	for {
		n := len(s)
		if n < 2 {
			break
		}
		first, last := s[0], s[n-1]
		pairs := map[byte]byte{'`': '`', '"': '"', '\'': '\''}
		want, ok := pairs[first]
		if !ok || last != want {
			break
		}
		s = strings.TrimSpace(s[1 : n-1])
	}
	s = argvBackslashEscapeRe.ReplaceAllString(s, "$1")
	return s
}

func normalizeInput(raw string) (NormalizedInput, error) {
	s := sanitizeArgvURL(raw)
	in, err := biliurl.Normalize(s)
	if err != nil {
		return NormalizedInput{}, fmt.Errorf("%w（支持 bilibili.com/video/BVxxx、b23.tv/xxx 或纯 BVxxx 链接，可带可不带 https://）", err)
	}
	return NormalizedInput{URL: in.URL, BVID: in.BVID}, nil
}

func normalizePathFlag(raw, flagName string) (string, error) {
	return pathx.Normalize(raw, flagName)
}
