# video-pipe

多协议流转换网关：接收任意输入源（文件 / RTMP / HTTP / RTSP / 摄像头），借助 **MediaMTX** 输出为 RTSP / RTMP / HLS / WebRTC / SRT，并提供一个 Web 管理界面做流的增删查、查看播放地址、查看实时状态。

> 设计原则：**不自己实现协议转换，复用 MediaMTX**。自研部分是"调度管理层"——每路流一个 FFmpeg 子进程的托管（看门狗 / 自动重启）、薄 JSON API、Web UI。详见 [`PRD.md`](./PRD.md)。

## 架构

```
浏览器 ──HTTP/JSON──▶ 后端 (Go, video-pipe)
                      ├─ server   /api/* + 页面
                      ├─ manager  生命周期 / 状态聚合
                      ├─ store    SQLite (流配置 = 唯一事实源)
                      └─ ffmpeg   每路一个进程 + 看门狗 ──推流(RTSP)──▶ MediaMTX ──▶ RTSP/RTMP/HLS/WebRTC/SRT
                          │
                          └─ mediamtx client  GET /v3/paths/get/{name} (online/readers)
```

- 每路活跃流由后端拉起一个 `ffmpeg` 进程，`-c copy` 无损转封装后 `rtsp://mediamtx:8554/<name>` 推给 MediaMTX；没有观众时不运行 FFmpeg。
- MediaMTX 的 `all_others.runOnDemand` 在 RTSP/RTMP/HLS/WebRTC/SRT 首个读者连接时通知后端启动；最后一个读者离开 30 秒后释放租约并停止 FFmpeg。后端 DB 仍是流定义的唯一事实源。
- 状态灯 = ffmpeg 进程状态 + MediaMTX `online`（每 5s 轮询 `/v3/paths/get/<name>`）。
- 看门狗：进程退出即按退避重启（`maxRestarts` 防死循环）；"活着但不出帧"（半开 TCP / 挂死摄像头）15s 内无 `progress` 心跳则杀进程重启。

## 快速开始（部署：拉取镜像，不构建）

镜像由 GitHub Actions 自动构建并发布到 GHCR（见下"CI / 镜像发布"）。部署机只需 Docker（含 docker compose），**不执行任何 build**。

1. 配置镜像地址（一次性）：

   ```bash
   cp .env.example .env
   # 编辑 .env：
   # - VIDEOPIPE_IMAGE 改成 ghcr.io/<你的用户名>/video-pipe:latest
   # - 线上部署还要设置 PLAYBACK_HOST 和 WEBRTC_ADDITIONAL_HOSTS
   ```

2. 拉取并启动：

   ```bash
   docker compose pull      # 拉取预构建镜像（video-pipe 服务 pull_policy: always）
   docker compose up -d
   ```

服务就绪后：

- 管理界面：<http://localhost:8080>
- HLS 和 WebRTC 信令由管理服务同源转发，无需对外开放 MediaMTX 的 8888/8889 端口。

**冒烟测试（无需任何外部视频源）**：打开管理界面，名称填 `demo`，类型选 `test`（地址留空），点"创建并准备"。流先显示“待播放”；点击 HLS/WebRTC 播放或用 VLC 打开 RTSP 地址后才启动测试图 FFmpeg，约 5 秒内状态灯变绿（在线）：

| 协议 | 地址（path=`demo`） |
|---|---|
| RTSP | `rtsp://localhost:8554/demo` |
| RTMP | `rtmp://localhost:1935/demo` |
| HLS  | `http://localhost:8080/playback/hls/demo/index.m3u8` |
| WebRTC | 在管理页点 WebRTC 的播放按钮（WHEP 路径：`/playback/webrtc/demo/whep`） |
| SRT   | `srt://localhost:8890?streamid=#!::m=request,r=demo` |

> 没有 FLV：MediaMTX 不输出 FLV，以上 5 种即全部；浏览器播放用 HLS 或 WebRTC。HLS/WebRTC 自动使用当前管理页的 HTTP(S) 域名；`PLAYBACK_HOST` 只用于生成 RTSP/RTMP/SRT 这些原生客户端地址。

接入真实摄像头：输入类型选“自动识别”或“RTSP”，地址填 `rtsp://user:pass@ip:554/...`。本地文件：输入类型选“本地文件”后，页面才会显示“选择视频文件”按钮；文件会上传到挂载的 `./data/uploads/`，无需手填路径。

**视频/直播站点（provider）**：输入类型直接选 `B站`，地址填 B站视频页（`https://www.bilibili.com/video/BV…`）或直播间（`https://live.bilibili.com/<房间>`）；选 `斗鱼（实验）` 时，地址填 `https://www.douyu.com/<房间号>`。页面不再单独展示“来源”字段，后端仍会自动把页面/房间地址解析成 CDN 直链再转封装：

- B站解析使用内置的纯 Go resolver（含 WBI 签名）；**斗鱼为实验性自研解析**（依赖站点的 `sign` 算法，可能随站点改版失效，需按真实房间联调）。
- 未带登录 cookie，只能拿到公开清晰度（通常标清/流畅）；HD/会员内容暂不可用。
- B站普通视频创建后会立即在后台下载到持久化缓存（最多同时下载 2 路），页面依次显示“下载中”与“待播放”；缓存完成后仍不启动 FFmpeg，直到有人观看才从本地文件循环推流。停用会保留缓存，删除时同步清理。B站/斗鱼直播不预下载，首次观看时解析直链并拉流。
- provider 视频会统一转码为 H.264 Baseline（禁用 B 帧）+ Opus，以保证 HLS/WebRTC 浏览器兼容性。这会比单纯转封装消耗更多 CPU。
- 直播直链会过期，断流/重启时会自动重新解析。

## 输出协议（可选）

MediaMTX 对每个流默认开 RTSP/RTMP/HLS/WebRTC/SRT 全部 5 种。设置环境变量 `ENABLE_RTSP` / `ENABLE_RTMP` / `ENABLE_HLS` / `ENABLE_WEBRTC` / `ENABLE_SRT` 为 `0`/`false`/`no` 可让 UI/API **不再展示**该协议的播放地址（默认全开）。要真正在 MediaMTX 侧关闭某协议，还需在 `mediamtx.yml` 里关闭它（如 `hls: false`、`webrtc: false`）；对 RTSP/RTMP/SRT 同时取消 Compose 中的对外端口发布。

### 线上 HLS / WebRTC 配置

HLS 与 WebRTC 的 HTTP 信令都从管理页的同源路径转发，因此即使管理页在 Nginx/Caddy 的 HTTPS 后面，也不会触发 mixed content 或 CORS。反向代理只需将整个 `/` 转发到 `video-pipe:8080`，不要排除 `/playback/`。

WebRTC 的媒体数据不走 HTTP 反代，需要：

1. 在 `.env` 中把 `WEBRTC_ADDITIONAL_HOSTS` 设为服务器的公网 IPv4 或 DNS 名。使用 CDN 时要填能直达源站的地址，不能填只代理 HTTP 的 CDN 节点。
2. 云安全组和宿主机防火墙放行 `8189/udp`；建议同时放行 `8189/tcp` 作为 UDP 被客户端网络拦截时的回退。
3. 重建 MediaMTX 容器使配置生效：`docker compose up -d --force-recreate mediamtx video-pipe`。

Docker bridge 网络可以继续使用，无需切换 `network_mode: host`。如果不能开放 ICE 端口，先使用 HLS；更严格的企业网络需要 TURN 服务器。

## 配置（环境变量）

`docker-compose.yml` 里 `video-pipe` 服务的环境变量：

| 变量 | 默认 | 说明 |
|---|---|---|
| `ADDR` | `:8080` | 管理 API/UI 监听地址 |
| `ON_DEMAND_ADDR` | `:8081` | MediaMTX 按需回调监听地址，仅容器网络使用，不要对公网发布 |
| `DB_PATH` | `/data/video-pipe.db` | SQLite 路径（挂载到宿主 `./data`） |
| `MEDIAMTX_API` | `http://mediamtx:9997` | MediaMTX 控制 API（容器内） |
| `MEDIAMTX_HOST` | `mediamtx` | ffmpeg 推流目标主机（容器内） |
| `MEDIAMTX_USER` / `MEDIAMTX_PASS` | `wrapper` / `change-me` | 控制 API Basic Auth（须与 `mediamtx.yml` 的 `authInternalUsers` 一致） |
| `PLAYBACK_HOST` | `localhost` | 生成 RTSP/RTMP/SRT 地址时使用的公网主机名/IP |
| `MEDIAMTX_HLS` / `MEDIAMTX_WEBRTC` | `http://mediamtx:8888` / `http://mediamtx:8889` | HLS 和 WebRTC 同源反代的容器内上游，Compose 部署通常无需修改 |
| `UPLOAD_DIR` | `/data/uploads` | 上传文件存放目录（挂载到宿主 `./data/uploads`） |
| `PROVIDER_CACHE_DIR` | `/data/provider-cache` | B站普通视频的完整下载缓存目录（挂载到宿主 `./data/provider-cache`） |
| `UPLOAD_MAX_BYTES` | `0` | 单个上传文件大小上限（字节），`0` 表示不限。超出返回 413 |
| `ENABLE_RTSP` / `ENABLE_RTMP` / `ENABLE_HLS` / `ENABLE_WEBRTC` / `ENABLE_SRT` | 全部启用 | 是否在 UI/API 展示对应协议的播放地址（`0`/`false`/`no` 关闭）。配合 `mediamtx.yml` 的 `xxx: false` 可真正关闭该协议 |

> `WEBRTC_ADDITIONAL_HOSTS` 是 MediaMTX 容器的环境变量，由 `docker-compose.yml` 从 `.env` 注入；它不是 Go 后端配置。

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET`  | `/api/streams` | 列出所有流（含状态、播放地址） |
| `POST` | `/api/streams` | 新增并启用一路按需流。Body：`{name, source_url, source_type, provider?}`（`provider` 可选：`bilibili`/`douyu`，此时 `source_url` 填页面/房间地址） |
| `POST` | `/api/uploads` | 上传源文件（multipart `file` 字段），返回 `{path, name, size}`；`path` 可作为 `/api/streams` 的 `source_url`（落盘到 `UPLOAD_DIR`） |
| `GET`  | `/api/streams/{name}/urls` | 返回该流各协议播放地址 |
| `GET`  | `/api/streams/{name}/status` | 单流状态 |
| `POST` | `/api/streams/{name}/start` | 重新启用一路已停用的流；进入待播放，不立即启动 FFmpeg |
| `POST` | `/api/streams/{name}/stop` | 停用一路流并立即停止 FFmpeg（保留配置和缓存） |
| `DELETE` | `/api/streams/{name}` | 删除一路流（停进程 + 删配置） |

示例：

```bash
# 创建一路 test 源流
curl -X POST localhost:8080/api/streams \
  -H 'Content-Type: application/json' \
  -d '{"name":"demo","source_type":"test"}'

# 查看状态
curl localhost:8080/api/streams
```

`source_type` 可选：`auto`（按 URL scheme 推导）/ `file` / `rtsp` / `rtmp` / `http` / `test`。`file` 源会以实时速率（`-re`）推送；其余视为直播源（异常退出会自动重启）。

## 目录结构

```
cmd/video-pipe/main.go        入口：装配依赖、信号处理、优雅退出
internal/config/               环境变量配置
internal/model/                Stream 领域模型 + 源类型推导
internal/store/                SQLite 持久化 + 迁移
internal/ffmpeg/               命令构造 + 进程托管（进程组清理、看门狗、退避重启）
internal/mediamtx/             MediaMTX 控制 API 客户端
internal/manager/              按需生命周期 + B站缓存准备 + 状态聚合
internal/ondemand/             MediaMTX 前台 helper（获取/心跳/释放观看租约）
internal/server/               HTTP API + html/template UI（templates/、static/ 内嵌）
mediamtx.yml                   MediaMTX 配置（开启 Control API + wrapper 鉴权）
Dockerfile                     镜像构建（CI 用）
docker-compose.yml             部署编排：拉取预构建镜像（pull，不 build）
.env.example                   部署配置模板（镜像、公网播放主机、WebRTC ICE 等）
.github/workflows/ci.yml       CI：test + 构建并推送镜像到 GHCR
```

## CI / 镜像发布

`.github/workflows/ci.yml` 在推送到 `master`、打 `v*` 标签或手动触发（workflow_dispatch）时：

1. **test** 任务：`go vet` + `go test ./...`（同时验证可编译）。
2. **build** 任务（依赖 test 通过）：构建 Docker 镜像并推送到 GHCR `ghcr.io/<owner>/<repo>`。

镜像标签（由 `docker/metadata-action` 生成）：
- `master` 分支 → `master`、`latest`、`sha-<短hash>`
- `v1.2.3` 标签 → `1.2.3`、`1.2`、`latest`、`sha-<短hash>`
- Pull Request → 仅构建不推送（验证可构建）

CI 使用内置 `GITHUB_TOKEN`（`packages: write`），无需额外密钥。首次推送后 GHCR 会生成 package；如需公开拉取，到 GitHub → Packages 把可见性改为 Public 并关联本仓库。默认构建 `linux/amd64`；边缘/ARM 场景在 workflow 里放开 `platforms: linux/amd64,linux/arm64` 即可（QEMU 跨架构构建会更慢）。

## 开发

本机无需安装 Go，镜像由 CI 发布。本地自测：

```bash
# 编译 + 静态检查 + 单元测试
docker run --rm -v "$PWD":/src -w /src golang:1.25-alpine \
  sh -c 'go mod tidy && go vet ./... && go test ./...'

# 从源码本地构建镜像（部署请直接用 CI 产物 + docker compose pull）
docker build -t video-pipe:dev .
```

单测覆盖：ffmpeg 命令构造（各源类型的参数、`-re`/`-rtsp_transport` 位置）。

### 验证清单

- [x] `test` 源建流 → 待播放 → 打开 RTSP/HLS 后状态灯转绿
- [ ] `enable`/`stop`/`delete` 操作工作正常
- [ ] 接入一个不存在的 RTSP 地址 → 重启计数增长，超限后转红(error)
- [ ] `kill -STOP` 某 ffmpeg 进程 → ~15s 后看门狗杀进程并重启
- [ ] 最后一个播放器退出 30 秒后 → FFmpeg 退出、状态回到待播放
- [ ] `docker compose restart video-pipe` → 流保持待播放；仍在线的 MediaMTX helper 心跳会重新拉起对应流

## 范围外（后续演进，见 PRD §9.6）

转码/水印/AI、鉴权与多租户、GB28181/ONVIF 适配、多节点/K8s。
