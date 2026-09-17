# bilibili-txt

从 Bilibili 视频 URL 自动生成文字稿的命令行工具。

**这个工具想解决什么**：现在 B 站上很多视频套路满满——3 分钟能讲完的内容硬拉成 30 分钟，靠反复复述、故弄玄虚、"先赞后看"式的话术骗完播率。`bilibili-txt` 把视频转成可扫读的文字稿，让你**用一分钟判断这条视频值不值得看完**：有干货就点开，全是水词就跳过，把时间还给自己。

- 优先抓官方 CC 字幕；没字幕才回退到本地 ASR（whisper.cpp）转录。
- Go 单二进制 + `os/exec` 编排 [yt-dlp](https://github.com/yt-dlp/yt-dlp) / [ffmpeg](https://ffmpeg.org/) / [whisper.cpp](https://github.com/ggml-org/whisper.cpp)。
- 输出默认按"一条字幕一行"排版，方便扫读；也支持 `md` / `srt`。

> 目前只在 **macOS（Apple Silicon）** 上验证过；Linux / Windows 理论可跑（Go 二进制 + 三个外部工具都跨平台），但下面的 `brew` 命令和 `--cookies-from-browser` 钥匙串弹窗仅适用于 macOS。

---

## 安装外部依赖（首次使用必读）

macOS：

```bash
brew install yt-dlp ffmpeg whisper-cpp
```

下载 whisper 大模型（走国内镜像，不用翻墙）：

```bash
mkdir -p ~/.local/share/whisper
curl -L -o ~/.local/share/whisper/ggml-large-v3-turbo.bin \
  https://hf-mirror.com/ggerganov/whisper.cpp/resolve/main/ggml-large-v3-turbo.bin
```

> 若 hf-mirror 拉不动，可以换 ModelScope 同款文件；模型放到 `config.yaml` 里 `model` 指向的路径即可（默认就是上面这个位置，通常不用改）。

---

## 使用

```bash
# 基本用法（自动判断走字幕分支或 ASR）
bilibili-txt 'https://www.bilibili.com/video/BVxxx/?spm_id_from=xxx'

# 强制走 ASR（当官方字幕质量差、想让 whisper 重新转一遍时用）
bilibili-txt --force-asr 'https://www.bilibili.com/video/BVxxx/'

# 只传 BVID（无 ? & 元字符，可以不加引号）
bilibili-txt -o ~/Documents/transcripts BVxxx

# 查看全部选项
bilibili-txt --help
```

> **URL 必须用单引号包起来**（`'...'`），不然 zsh 会把 `? & ` `` ` `` 之类的字符当通配符 / 命令替换处理，程序根本收不到完整 URL。原理和更多写法见下面的 **Troubleshooting** 段。单引号里怎么写（原生 `?`、`\?` 转义、外层再包一层反引号……）都能被程序自动认，不用纠结。纯 BVID 或者去掉 `?` 参数的 URL 本身不含 zsh 元字符，加不加引号都行。

外部工具路径若不在 `$PATH`，可在**当前执行目录下的 `config/config.yaml`** 里的 `binaries.*` 显式指定。该配置文件是**可选**的：不存在时直接用内置默认值。**没有 CLI flag**，就地配置或走 PATH，二选一。

模板参考仓库内的 `config/config.example.yaml`。

---

## 图形界面（浏览器）

不想敲命令行、也不想跟 shell 引号较劲，可以直接**不带任何参数**启动，它会在本地起一个小服务并自动打开浏览器界面：

```bash
bilibili-txt
```

界面是三栏布局：

- **顶部输入框**：直接把浏览器地址栏 / 分享面板里的链接整段粘进去即可，**不用加引号**，`https://` 协议头漏了也能认；回车或点「转换」开始。进行中按钮变成「取消」，可随时中断。
- **左栏**：实时滚动日志（yt-dlp / ffmpeg / whisper 的输出也会在这里）。
- **中栏**：完成后的文字稿，自动换行、可上下滚动；点右栏的历史记录也能回看旧稿。
- **右栏**：最近转换的 **5 个**视频，按时间倒序排列；超过 5 个会自动删掉最旧的（命令行方式产出的文稿同样计入、同样会被清理）。

几点说明：

- 服务只监听本机 `127.0.0.1` 的一个**随机端口**，并带一次性访问令牌（仅注入到本地页面），局域网其它设备访问不了。
- 服务在终端**前台**运行，回到终端按 **Ctrl-C** 退出；关闭浏览器窗口后，服务会在短时间内自动结束。
- 默认用系统默认浏览器以新标签页打开界面。
- 界面模式只读取 `config/config.yaml`，不接受 `--force-asr` / `--keep-intermediate` / `--overwrite` / `--skip` / `-o` 这些只对单次转换有意义的参数（和它们一起用会直接报错）。

界面相关的可选配置（同样写在 `config/config.yaml`）：

```yaml
server:
  port: 0            # 监听端口；0 = 随机端口（默认）
  open_browser: true # 启动后是否自动打开浏览器
```

---

## 常见问题 / Troubleshooting

### `zsh: no matches found: https://...` —— 请给 URL 加单引号

Bilibili 分享出来的完整链接通常长这样：

```
https://www.bilibili.com/video/BVxxx/?param1=xxx&param2=yyy
```

`?` 和 `[` 是 zsh 的通配符（glob）；未加引号时 zsh 会在**参数展开阶段**把 URL 当模式去匹配文件，匹配不到就直接抛 `no matches found`——`bilibili-txt` 二进制**根本没被启动**。反引号 `` ` `` 更危险：zsh 会把它当作命令替换。

这属于 shell 层行为，Go 程序运行时无法拦截；但**只要外层套一对单引号让 URL 原样进 argv**，程序内部会容错以下几种复制粘贴变体，你不用再纠结：

- 浏览器地址栏原样复制：`'https://.../BVxxx/?a=1&b=2'`
- 文档 / cURL 里带反斜杠转义的形式：`'https://.../BVxxx/\?a=1\&b=2'`
- 首尾包了反引号：`` '`https://.../BVxxx/?a=1`' ``
- 引号套嵌 + 反斜杠转义混用：`'"https://.../BVxxx/\?a=1"'`

**正确写法**（推荐第 1 种，其余按需）：

```zsh
# 1. 单引号（推荐，且是 Usage 段统一示范的写法）
bilibili-txt 'https://www.bilibili.com/video/BVxxx/?param1=xxx'

# 2. 双引号也行（URL 里含 $ 或 反引号时不安全）
bilibili-txt "https://www.bilibili.com/video/BVxxx/?param1=xxx"

# 3. 去掉 ? 后面的追踪参数，URL 里没 zsh 元字符了，可以不加引号
bilibili-txt https://www.bilibili.com/video/BVxxx/

# 4. 只传 BVID
bilibili-txt BVxxx

# 5. 用 `noglob` 前缀一次性关掉 zsh 通配符（zsh 专属）
noglob bilibili-txt https://www.bilibili.com/video/BVxxx/?param1=xxx
```

嫌每次都加引号麻烦，可以在 `~/.zshrc` 里配一次 alias 一劳永逸：

```zsh
alias bilibili-txt='noglob /absolute/path/to/bilibili-txt'
```

（`noglob` 是 zsh 特有的关键字，bash / fish 上不适用。）

### 首次运行弹了个"允许访问钥匙串"的窗？

预期行为。默认配置下 yt-dlp 会走 `--cookies-from-browser chrome` 读取 Chrome 的登录 cookie，让你能拉需要登录才可见的视频（比如高码率、部分番剧片头）。macOS 首次读 Chrome cookie 时钥匙串会弹一次授权框，点 **"始终允许"** 即可，后续不会再弹。

如果不需要登录态（只想拉完全公开的视频），把 `config/config.yaml` 里改成：

```yaml
auth:
  cookies_from_browser: none    # 或者换成 firefox / edge / safari 等
```

`none`（不区分大小写）= 完全跳过 `--cookies-from-browser`；写其它浏览器名 = 从对应浏览器读 cookie。留空则回落到默认 chrome。

### 日志去哪里了？

默认情况下**只输出到 stderr**（就是你终端里看到的那些 `time=... level=INFO ...`）；`config/config.yaml` 里 `logging.file` 为空且 `debug: false` 时不会生成任何日志文件。

想同时留一份到磁盘：

```yaml
logging:
  file: ./logs/main.log     # 主日志 append 到该路径（stderr 仍照打）
debug: true                 # 生成外部命令（yt-dlp/ffmpeg/whisper）stderr 全量抓取文件
                            # 默认落到 ./logs/<BVID>-<时间戳>.log
```

---

## 本地开发

**不需要装 yt-dlp / ffmpeg / whisper-cli 也能开发和测试**（内部走 fake 二进制）：

```bash
# 全量单测 + 集成测
go test ./... -cover

# 静态检查
go vet ./...
gofmt -l -w .

# 开发期构建（不注入版本号，--version 打印 0.0.0-dev）
go build -o bin/bilibili-txt ./cmd/bilibili-txt

# 正式构建（自动从 git describe 取版本注入到 --version）
./scripts/build.sh

# 手动指定版本
VERSION=v1.2.3 ./scripts/build.sh

# 交付前一键体检：gofmt + vet + test -race -count=1
./scripts/check.sh
```

真跑一次端到端（需先装真工具）：

```bash
BILIBILI_TXT_E2E=1 go test -tags=e2e ./test/e2e/...
```

---

## 免责声明

**这是个人自用工具**，公开源码仅供学习交流。

- 使用前请自行确认所在地区法律及 [Bilibili 用户使用协议](https://www.bilibili.com/protocal/licence.html) 允许你这样处理视频内容。工具仅在**你有权访问、且个人非商业用途**的前提下使用；作者不承担因误用产生的任何账号封禁、内容侵权或法律责任。
- **不要**用它抓取付费内容、大会员专享内容或番剧付费剧集；**不要**把生成的文字稿以任何形式二次分发（发公众号、知乎、小红书、B 站专栏等均属再发行原作者版权内容）。
- 工具默认走 `--cookies-from-browser chrome` 复用你自己浏览器里的登录态，请求特征等同于普通浏览行为；即便如此，**高频调用仍可能触发风控**（限流 / 临时封号），请自行控制频率。
- 依赖的第三方工具（[yt-dlp](https://github.com/yt-dlp/yt-dlp) / [ffmpeg](https://ffmpeg.org/) / [whisper.cpp](https://github.com/ggml-org/whisper.cpp)）各有各的 license，本仓库不打包也不再分发它们的代码，只在运行时通过 `os/exec` 调用。

