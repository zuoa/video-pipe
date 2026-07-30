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

- 每路流由后端拉起一个 `ffmpeg` 进程，`-c copy` 无损转封装后 `rtsp://mediamtx:8554/<name>` 推给 MediaMTX。
- MediaMTX 默认 `all_others` 即时接受 publisher，**无需预先建 path**；后端 DB 是流定义的唯一事实源。
- 状态灯 = ffmpeg 进程状态 + MediaMTX `online`（每 5s 轮询 `/v3/paths/get/<name>`）。
- 看门狗：进程退出即按退避重启（`maxRestarts` 防死循环）；"活着但不出帧"（半开 TCP / 挂死摄像头）15s 内无 `progress` 心跳则杀进程重启。

## 快速开始（部署：拉取镜像，不构建）

镜像由 GitHub Actions 自动构建并发布到 GHCR（见下"CI / 镜像发布"）。部署机只需 Docker（含 docker compose），**不执行任何 build**。

1. 配置镜像地址（一次性）：

   ```bash
   cp .env.example .env
   # 编辑 .env，把 VIDEOPIPE_IMAGE 改成 ghcr.io/<你的用户名>/video-pipe:latest
   ```

2. 拉取并启动：

   ```bash
   docker compose pull      # 拉取预构建镜像（video-pipe 服务 pull_policy: always）
   docker compose up -d
   ```

服务就绪后：

- 管理界面：<http://localhost:8080>
- MediaMTX 控制 API：<http://localhost:9997>

**冒烟测试（无需任何外部视频源）**：打开管理界面，名称填 `demo`，类型选 `test`，地址留空，点"创建并启动"。约 5 秒内状态灯变绿（在线），即可复制各协议地址到 VLC / Safari 验证：

| 协议 | 地址（path=`demo`） |
|---|---|
| RTSP | `rtsp://localhost:8554/demo` |
| RTMP | `rtmp://localhost:1935/demo` |
| HLS  | `http://localhost:8888/demo/index.m3u8` |
| WebRTC | `http://localhost:8889/demo`（浏览器打开） |
| SRT   | `srt://localhost:8890?streamid=#!::m=request,r=demo` |

接入真实摄像头：类型选 `auto` 或 `rtsp`，地址填 `rtsp://user:pass@ip:554/...`。本地文件：类型选 `auto` 或 `file`，地址填容器内路径（挂载到 `./data`，如 `/data/sample.mp4`）。

## 配置（环境变量）

`docker-compose.yml` 里 `video-pipe` 服务的环境变量：

| 变量 | 默认 | 说明 |
|---|---|---|
| `ADDR` | `:8080` | 管理 API/UI 监听地址 |
| `DB_PATH` | `/data/video-pipe.db` | SQLite 路径（挂载到宿主 `./data`） |
| `MEDIAMTX_API` | `http://mediamtx:9997` | MediaMTX 控制 API（容器内） |
| `MEDIAMTX_HOST` | `mediamtx` | ffmpeg 推流目标主机（容器内） |
| `MEDIAMTX_USER` / `MEDIAMTX_PASS` | `wrapper` / `change-me` | 控制 API Basic Auth（须与 `mediamtx.yml` 的 `authInternalUsers` 一致） |
| `PLAYBACK_HOST` | `localhost` | **浏览器**访问 MediaMTX 的主机名，用于拼播放地址（容器内地址浏览器不可达，必须改为对外可达地址） |

> 生产部署：把 `PLAYBACK_HOST` 改成对外域名/IP；如需对外暴露，按需在 `docker-compose.yml` 发布端口。

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET`  | `/api/streams` | 列出所有流（含状态、播放地址） |
| `POST` | `/api/streams` | 新增并启动一路流。Body：`{name, source_url, source_type}` |
| `GET`  | `/api/streams/{name}/urls` | 返回该流各协议播放地址 |
| `GET`  | `/api/streams/{name}/status` | 单流状态 |
| `POST` | `/api/streams/{name}/start` | 启动一路已停止/出错的流 |
| `POST` | `/api/streams/{name}/stop` | 停止一路流（保留配置） |
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
internal/manager/              生命周期 + 启动恢复 + 状态聚合
internal/server/               HTTP API + html/template UI（templates/、static/ 内嵌）
mediamtx.yml                   MediaMTX 配置（开启 Control API + wrapper 鉴权）
Dockerfile                     镜像构建（CI 用）
docker-compose.yml             部署编排：拉取预构建镜像（pull，不 build）
.env.example                   镜像地址配置模板（VIDEOPIPE_IMAGE）
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

- [x] `test` 源建流 → 状态灯转绿 → RTSP/HLS 在 VLC 播放
- [ ] `start`/`stop`/`delete` 按钮工作正常
- [ ] 接入一个不存在的 RTSP 地址 → 重启计数增长，超限后转红(error)
- [ ] `kill -STOP` 某 ffmpeg 进程 → ~15s 后看门狗杀进程并重启
- [ ] `docker compose restart video-pipe` → running 状态的流自动恢复

## 范围外（后续演进，见 PRD §9.6）

转码/水印/AI、鉴权与多租户、按需启停（`runOnDemand`）、GB28181/ONVIF 适配、多节点/K8s、内嵌播放器预览。
