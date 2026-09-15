# webdav-mux

webdav-mux 是一个只读的 WebDAV 聚合服务。它自己不存储任何内容，而是把若干个“上游”（远程 WebDAV 服务，或者本机上的目录）的子目录，按账号拼成各自的目录树，再以标准 WebDAV 协议提供给客户端。客户端的所有流量都经由本服务转发，不会被重定向到上游。

所有配置（上游、账号、挂载关系）都写在一个 YAML 文件里，不依赖任何数据库。

## 概念

- **上游（upstream）**：内容的来源，二选一：
  - 远程 WebDAV 服务，用 `url` 指定，可选 Basic 认证，可选经由下载代理获取文件（见“下载代理”）；
  - 本地目录，用 `dir` 指定。本服务在进程内把它包装成一个 WebDAV 上游，与远程上游走完全相同的逻辑。
- **账号（user）**：客户端用 HTTP Basic 认证登录的身份。不同账号看到不同的目录树。
- **挂载（mount）**：`客户端路径: 上游名:上游路径`，把某个上游里的一个目录放到账号目录树的某个位置上。

例如下面的配置：

```yaml
users:
  alice:
    mounts:
      /movies: nas:/media/movies
      /work/docs: cloud:/Documents
      /work/share: nas:/share/team
```

alice 登录后看到：

```
/                 ← 本服务合成
├── movies/       → nas 上的 /media/movies
└── work/         ← 本服务合成（挂载点的上级目录）
    ├── docs/     → cloud 上的 /Documents
    └── share/    → nas 上的 /share/team
```

所有账号访问同一个地址（例如 `https://dav.example.com/`），看到什么内容取决于用哪个账号登录。

## 快速开始

需要 Go 1.27.1 或更高版本。

```sh
# 1. 构建。CGO_ENABLED=0 得到静态链接的单文件，可以直接拷到 NAS 等没有 Go 环境的机器上运行；
#    -trimpath -ldflags="-s -w" 去掉构建路径、符号表和调试信息，二进制约小三分之一
#    （交叉编译时再加 GOOS/GOARCH，例如 GOOS=linux GOARCH=arm64）
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o webdav-mux .

# 2. 为每个账号生成密码哈希（交互式输入；也可以 echo 'password' | ./webdav-mux hash-password）
./webdav-mux hash-password

# 3. 参照 config.example.yaml 编写 config.yaml，然后校验
./webdav-mux check -config config.yaml

# 4. 启动
./webdav-mux serve -config config.yaml
```

之后在客户端里添加 WebDAV 服务器 `http://主机:8080/`（启用 TLS 时为 `https://`），使用配置里的账号密码登录。

修改配置后，可以向进程发送 `SIGHUP` 热加载：

```sh
./webdav-mux check -config config.yaml && kill -HUP <pid>
```

新配置校验失败时继续使用旧配置，并在日志里报错。`listen` 和“是否启用 TLS”在启动时确定，修改它们需要重启；证书文件本身会在热加载时重新读取，可用于更换证书。

## 使用 Docker

用仓库里的 [Dockerfile](Dockerfile) 构建的镜像基于 `scratch`，里面只有一个静态链接、去掉了符号表和调试信息的二进制，以及 CA 证书（访问 HTTPS 上游时校验证书用）。按架构不同，镜像压缩后约 3.5 MiB，解压后 8 到 9 MiB。镜像的约定：

- 默认命令是 `serve -config /etc/webdav-mux/config.yaml`，把配置文件挂载到这个路径即可。
- 在容器内以 UID/GID `65532` 运行。挂载进去的配置文件、证书和本地目录需要对这个 UID 可读，否则用 `docker run --user` 换成有权限的 UID。
- 声明的端口是 8080。配置里的 `listen` 要监听所有地址（默认的 `:8080` 即可），写成 `127.0.0.1:8080` 的话容器外访问不到。
- 镜像里没有时区数据，日志时间是 UTC。

### 构建单一架构的镜像

```sh
docker build -t webdav-mux .
```

得到的镜像与执行构建的机器架构相同。

### 构建多架构镜像

需要 Docker Buildx（Docker Desktop 和较新的 Docker Engine 都自带）。Dockerfile 的编译阶段固定在构建机的原生架构上运行，用 Go 交叉编译产出各个目标架构的二进制，所以构建其他架构的镜像**不需要安装 QEMU**，速度和本机构建差不多。

1. 创建一个使用 `docker-container` 驱动的构建器。只需执行一次。Docker 默认的构建器在没有启用 containerd 镜像存储时，不能构建多架构镜像。

   ```sh
   docker buildx create --name multiarch --driver docker-container --use
   ```

2. 构建并推送到镜像仓库。多架构镜像是一个清单列表（manifest list），要直接推送到仓库（Docker Hub、GHCR、自建 registry 等，先 `docker login`）：

   ```sh
   docker buildx build \
     --platform linux/amd64,linux/arm64,linux/arm/v7 \
     -t registry.example.com/yourname/webdav-mux:latest \
     --push .
   ```

   `--platform` 按需增减，Go 支持的 Linux 架构都可以用：
   - `linux/amd64`：常见的 PC 和服务器；
   - `linux/arm64`：树莓派 3/4/5 的 64 位系统、苹果芯片、多数 ARM NAS；
   - `linux/arm/v7`、`linux/arm/v6`：32 位 ARM 设备；
   - `linux/386`、`linux/ppc64le`、`linux/s390x`、`linux/riscv64` 等。

3. 查看镜像包含哪些架构：

   ```sh
   docker buildx imagetools inspect registry.example.com/yourname/webdav-mux:latest
   ```

   在目标机器上 `docker pull` 时，Docker 会自动选择与该机器架构匹配的那一份。

没有镜像仓库时，可以按目标机器的架构单独构建，导出成文件拷过去。`--load` 一次只能导入一个架构，除非本机 Docker 启用了 containerd 镜像存储：

```sh
docker buildx build --platform linux/arm64 -t webdav-mux:arm64 --load .
docker save webdav-mux:arm64 | gzip > webdav-mux-arm64.tar.gz

# 在目标机器上
gunzip -c webdav-mux-arm64.tar.gz | docker load
```

编译用的 Go 版本默认与 `go.mod` 一致，可以用 `--build-arg GO_VERSION=<版本>` 覆盖，但不能低于 `go.mod` 要求的版本。

构建上下文由 [.dockerignore](.dockerignore) 按白名单放行 `go.mod`、`go.sum` 和 Go 源码（规则的边界情况见文件内的注释），本地的 `config.yaml`、证书私钥等文件不会被发送给构建器。新增顶层源码目录，或者 `go:embed` 之类的非 `.go` 构建输入时，要同步修改 `.dockerignore`，否则镜像构建会因为缺文件而失败。

### 运行容器

```sh
docker run -d --name webdav-mux --restart unless-stopped \
  -p 8080:8080 \
  -v /path/to/config.yaml:/etc/webdav-mux/config.yaml:ro \
  -v /srv/media:/srv/media:ro \
  registry.example.com/yourname/webdav-mux:latest
```

- 配置里本地上游的 `dir` 写的是**容器内**的路径，对应的目录要用 `-v` 挂载进去，建议加 `:ro` 只读挂载。
- 启用 TLS 时，证书和私钥同样挂载进容器，`tls.cert`、`tls.key` 写容器内的路径。
- 热加载配置：`docker kill -s HUP webdav-mux`。
- 校验配置：

  ```sh
  docker run --rm -v /path/to/config.yaml:/etc/webdav-mux/config.yaml:ro \
    registry.example.com/yourname/webdav-mux:latest check -config /etc/webdav-mux/config.yaml
  ```

  配置里引用的本地目录也要一并挂载，否则校验会报目录不存在。
- 生成密码哈希：`docker run --rm -it registry.example.com/yourname/webdav-mux:latest hash-password`。

### 使用 Docker Compose

仓库里的 [docker-compose.yaml](docker-compose.yaml) 是一份可直接使用的模板：把配置放在 `./webdav-mux-data/config.yaml`，整个目录以只读方式挂载到容器的 `/etc/webdav-mux`，证书也放在这个目录里。文件里的注释说明了每一项的用意，按需修改镜像名、`user`、端口和本地目录挂载，然后：

```sh
docker compose run --rm webdav-mux check -config /etc/webdav-mux/config.yaml   # 校验配置
docker compose up -d                                                           # 启动
docker compose kill -s HUP webdav-mux                                          # 热加载配置
docker compose run --rm webdav-mux hash-password                               # 生成密码哈希
```

## 配置参考

完整示例见 [config.example.yaml](config.example.yaml)。配置里出现未知字段会直接报错，避免拼错的字段被悄悄忽略。

### 顶层

| 字段 | 说明 |
|---|---|
| `listen` | 监听地址，默认 `:8080`。 |
| `tls.cert`、`tls.key` | 可选。PEM 格式的证书链和私钥文件；配置后以 HTTPS 提供服务。 |
| `upstreams` | 上游表，键是上游名（字母、数字、`_`、`.`、`-`，以字母或数字开头）。 |
| `users` | 账号表，键是用户名（不能含 `:`）。 |

### 上游

每个上游必须恰好设置 `url` 或 `dir` 之一。

| 字段 | 说明 |
|---|---|
| `url` | 远程 WebDAV 服务的根地址，`http` 或 `https`，可以带路径（如 `https://host/remote.php/dav/files/bob`），不能带查询参数和用户名密码。 |
| `username`、`password` | 可选。访问该上游时使用的 Basic 认证凭据。 |
| `insecure_skip_verify` | 可选，默认 `false`。上游使用自签名证书时设为 `true`。只对该上游自身的地址生效，跟随重定向到其他地址、访问下载代理时仍然校验证书。 |
| `proxy` | 可选。下载代理的基地址，`http` 或 `https`，可以带路径，不能带查询参数和用户名密码。设置后获取文件内容经由该代理，列目录仍然直连上游，见“下载代理”。 |
| `dir` | 本地目录的绝对路径，加载配置时必须存在。本地上游不能设置 `username`、`password`、`insecure_skip_verify`、`proxy`。 |

### 账号

| 字段 | 说明 |
|---|---|
| `password_hash` | bcrypt 哈希。用 `webdav-mux hash-password` 生成，或取 `htpasswd -nB 用户名` 输出中冒号后面的部分。 |
| `mounts` | 挂载表，形如 `/客户端路径: 上游名:/上游路径`。 |

挂载规则：

- 两边的路径都要写成规范形式：以 `/` 开头，不含空段、`.`、`..`。路径按字面书写，不做百分号解码（`/100%` 就是名为 `100%` 的目录）。
- 上游路径相对于上游的根：远程上游相对 `url`，本地上游相对 `dir`。
- 挂载目标应当是目录。
- 同一账号的挂载路径不能互为前缀：`/a` 和 `/a/b` 不能同时存在；挂到 `/` 时只能有这一个挂载。
- 挂载路径之上的目录（如上例的 `/` 和 `/work`）由本服务自动合成。

## 行为说明

### 支持的方法

服务是只读的，只接受 `OPTIONS`、`GET`、`HEAD`、`PROPFIND`。其他方法（`PUT`、`DELETE`、`MKCOL`、`COPY`、`MOVE`、`PROPPATCH`、`LOCK` 等）一律返回 `405 Method Not Allowed`，不会到达上游。

`OPTIONS` 不需要认证，响应头声明 `DAV: 1`，即不支持锁。macOS Finder 看到这个会自动以只读方式挂载。

### PROPFIND

- `Depth` 只接受 `0` 和 `1`。`Depth: infinity` 和缺省的 `Depth`（RFC 4918 规定缺省等同于 infinity）返回 `403`，响应体带 `DAV:propfind-finite-depth` 前置条件。这样一个请求就不会让上游遍历整棵目录树。
- **合成目录**：本服务直接回答，返回固定属性：`resourcetype`（collection）、`displayname`、`getlastmodified`、`creationdate`（取配置加载时间）。不会访问上游，所以某个上游不可用时目录树本身仍可浏览。挂载点在上级目录的列表里也按合成目录显示。
- **挂载点之下**：请求体和 `Depth` 转发给上游。上游返回的 multistatus 以流式方式改写：
  - 只替换 `DAV:href` 的值，其余字节原样保留，所以 ETag、厂商自定义属性等都无损透传；
  - 挂载根那一条中，状态为 200 的 `DAV:displayname` 会替换成挂载点的名字（挂载在根上时替换为空）。这样目录名在列表里和进入目录后保持一致，也不会暴露上游目录的真实名字；
  - 指向挂载目标之外的 href 不会出现在响应里：
    - 顶层 href 在挂载目标之外的条目会被丢弃，并在日志里输出 `dropped PROPFIND entries` 警告，附带示例 href 和期望的前缀；
    - 属性值里引用了挂载目标之外资源的属性会被整个删除，客户端看到的效果是上游没有返回这个属性。例如 `DAV:current-user-principal` 会暴露上游的账号名，会被删除；
    - `DAV:location`、`DAV:error` 等元素里的这类 href 也会被删除。
- 上游返回的 href 必须以配置的 `url` 路径开头。如果上游放在一个会改写路径前缀的反向代理后面，返回的 href 就和访问地址对不上，所有条目都会被丢弃。遇到上面的警告时，先检查这一点。
- 客户端拿到的 href 除字母、数字和 `-._~` 外一律百分号编码。

### GET / HEAD

- 响应体流式转发，不缓冲。`Range`、`If-Range`、`If-None-Match`、`If-Modified-Since` 等条件请求头原样转发，所以播放器可以正常拖动进度。
- 客户端没有发 `Accept-Encoding` 时，本服务向上游声明 `identity`，保证 `Content-Length`、`Content-Range` 与实际内容一致。
- 合成目录上的 `GET` 返回 `405`。
- 上游在传输中途出错时，本服务直接中断客户端连接，客户端不会把截断的内容当成完整文件。

### 下载代理

远程上游可以用 `proxy` 配置一个下载代理。代理是一个独立的服务：本服务把文件的完整 URL 交给它，由它取回内容后流式返回，例如用多连接分片下载绕过上游的单连接限速。本服务与代理之间的协议定义在 [docs/proxy-protocol.md](docs/proxy-protocol.md)。

- 只有 `GET`、`HEAD` 经由代理，`PROPFIND` 始终直连上游。代理不可用时目录仍然可以浏览，但文件打不开。
- 协议请求为 `{proxy}/proxy?url={文件的上游 URL}`，`proxy` 末尾的 `/` 会先被去掉。代理可以用基地址的路径区分自己内部的配置，例如 `http://10.0.0.2:8090/profiles/fast`。
- 发给代理的请求头与直连时使用同一份白名单；上游的 Basic 凭据放在发给代理的 `Authorization` 里。响应头同样按直连时的白名单过滤。
- 重定向可以由代理自己跟随，也可以由代理原样返回、由本服务跟随。后一种情况下，本服务把 `Location` 相对文件的上游 URL 解析，再为新地址发起一次协议请求，并沿用直连时的规则：最多 10 次，凭据只随目标与上游 `url` 同源的请求发给代理。
- 代理用 `Proxy-Status` 响应头（RFC 9209）区分自身的错误和上游的响应，错误码见下文。
- 文件的 `ETag`、`Last-Modified` 由代理决定是否提供，可能与 `PROPFIND` 列表里的 `getetag`、`getlastmodified` 不同。协议要求不提供校验值的代理忽略条件请求头，所以客户端用列表里的值发条件请求不会出错。

### 转发与隔离

- 请求头和响应头都只按白名单转发。客户端的 `Authorization`、`Cookie` 不会发给上游；上游的 `Set-Cookie`、`WWW-Authenticate`、`Location` 以及其他内部头不会回给客户端。所有响应都带 `Cache-Control: private`，因为同一个 URL 在不同账号下是不同的内容。
- 不转发查询参数。
- 上游返回的重定向由本服务自己跟随（经由下载代理时见“下载代理”），最多 10 次，不会回给客户端：
  - `GET`、`HEAD` 跟随 301/302/303/307/308；`PROPFIND` 跟随 301/302/307/308，并保持方法和请求体。
  - 上游凭据只发给与配置的 `url` 同源（scheme、主机、端口都相同）的地址。例如上游 302 跳转到 CDN 直链时，内容照常中转，但不携带上游密码。
- 客户端路径先在自己的目录树里消解 `.` 和 `..`，然后才匹配挂载点，因此无法越过挂载目标。解码后的路径段如果可能被有缺陷的上游重新解释成目录穿越，请求直接返回 `400`，包括以下几类：
  - 含 `%2F`、`%5C` 且拆开后出现 `..`；
  - 多重编码的 `..`；
  - 只由点和空格组成（Windows 会把 `.. ` 当成 `..`）；
  - 用过长编码的 UTF-8（如 `%C0%AE`）表示点或斜杠；
  - 含全角点、全角斜杠、`¥` 等字符，且经 NFKC 归一化或 Windows best-fit 映射后会出现 `..`；
  - 含 NUL。

  只有重新解释后真的会出现点段才拒绝，`作品／第1話.mkv`、`¥100.txt` 这样的普通文件名不受影响。
- 本地目录通过 Go 的 `os.Root` 访问，任何路径都无法逃出 `dir`：
  - 指向 `dir` 之外的符号链接、悬空的符号链接、绝对路径形式的符号链接，都当作不存在，不会出现在列表里；
  - 指向 `dir` 之内的相对符号链接可以正常访问；
  - 每次操作都重新打开 `dir`，启动后才挂载到该路径上的磁盘也能立即看到。

### 错误码

| 情况 | 返回给客户端 |
|---|---|
| 上游返回 401 / 407（即上游凭据错误） | `502`。不能原样返回 401，否则客户端会以为自己的密码错了。 |
| 上游返回其他 4xx | 原样返回；`405` 附带过滤后的 `Allow`，`416` 附带 `Content-Range`，`429` 附带 `Retry-After` |
| 上游返回 503 / 504 | 原样返回 |
| 上游返回其他 5xx、无法跟随的 3xx、非 XML 的 PROPFIND 响应 | `502` |
| 连不上上游或下载代理 | `502`；建立连接超时（10 秒）或等待响应头超时（2 分钟）为 `504` |
| 下载代理报告自身错误（响应带含 `error` 参数的 `Proxy-Status`） | `error` 为 `dns_timeout`、`connection_timeout`、`connection_read_timeout`、`http_response_timeout` 时 `504`，其他 `502`。代理没有带这个头的响应，按上面几行的上游响应处理。 |

上游的错误响应体不会转发给客户端。

### 认证

- 客户端认证只支持 HTTP Basic。密码以 bcrypt 哈希存放在配置里。
- 一次成功的校验会缓存 10 分钟。WebDAV 客户端每个请求都带认证，不缓存的话每个请求都要付出一次 bcrypt 的开销。缓存在配置重载后失效。
- 未命中缓存的 bcrypt 校验有并发上限（CPU 核数的一半，至少 1），暴力破解不会拖垮正常的转发流量。
- 不存在的用户名同样会做一次 bcrypt 比较，代价取配置中最高的那个（上限 14），这样响应时间不容易暴露用户名是否存在。这一点只在所有账号使用相同 bcrypt 代价时严格成立；`hash-password` 默认生成的都是代价 10。如果混用了不同代价的哈希，代价较低的已有账号会比不存在的用户名响应得快。

### 日志

访问日志和错误写到标准错误，格式为 `log/slog` 的文本格式。每个请求一行，包含方法、路径、状态码、字节数、耗时、用户、上游及上游状态码；经由下载代理的请求带 `via_proxy=true`。出错的请求以 `WARN` 级别记录，并附带原因，下载代理报告的自身错误会记录其错误类型。

## 部署建议

- **直接暴露在公网时请启用 TLS**（`tls.cert`、`tls.key`），否则 Basic 认证的密码是明文传输的。另外，Windows 自带的 WebDAV 客户端默认只允许在 HTTPS 上使用 Basic 认证。
- 本服务生成的 href 以 `/` 为根。如果放在反向代理后面，并且挂在子路径下（如 `https://host/dav/`），反向代理除了改写请求路径，还需要改写 PROPFIND 响应里的 href。本服务不处理这种情况。
- 客户端连接的超时设置：
  - 请求头必须在 10 秒内收完；
  - 从收到请求到开始转发（认证、读取 PROPFIND 请求体）不能超过 30 秒，极慢地发送请求体的连接会被断开；
  - 开始转发之后不限时长；
  - 空闲的长连接 2 分钟后关闭。
- 下载代理会按请求获取任意 URL，应当部署在本服务能访问、外部无法访问的网络里，例如同一台机器或同一内网。协议不定义代理自身的认证；本服务与代理之间使用明文 HTTP 时，上游凭据在这段链路上也是明文的。
- 上游（以及下载代理）的超时设置：
  - 建立连接和 TLS 握手：各 10 秒；
  - 等待响应头：2 分钟；
  - 响应体传输：不限时长（视频流可以持续数小时），客户端断开时会立即取消对应的上游请求。

## 已知限制

- 不支持 `Depth: infinity`，不支持锁（LOCK/UNLOCK），不支持任何写操作。
- 挂载路径不能互为前缀，也就是不能把一个挂载嵌套在另一个挂载里面。
- 上游返回的 multistatus 必须是 UTF-8（或兼容 UTF-8 的 US-ASCII）编码；单个 `DAV:response` 元素（以及 response 之外的单个 XML 节点）不能超过 8 MiB。
- 上游认证只支持 Basic。
- 以下文件名无法访问：只由点和空格组成的、含 NUL 的，以及按上面“转发与隔离”一节的规则重新解释后会出现点段的。
- 引用了挂载目标之外资源的属性（如 `DAV:current-user-principal`、带 owner 链接的 `DAV:lockdiscovery`）不会返回给客户端。
- 本服务只识别 `DAV:href` 元素里的路径；上游如果把路径写在普通文本属性里（例如错误描述文字），不会被识别和改写。

## 开发

```sh
go test -race ./...
```

代码结构：

| 路径 | 内容 |
|---|---|
| `main.go` | 命令行：`serve`、`check`、`hash-password`，信号处理与热加载 |
| `internal/config` | YAML 解析与校验 |
| `internal/davpath` | 基于路径段的解析、编码与安全检查 |
| `internal/davxml` | multistatus 流式改写、PROPFIND 请求体解析、合成目录的响应生成 |
| `internal/localdav` | 本地目录上游：只读的 `os.Root` 文件系统和进程内 `http.RoundTripper` |
| `internal/server` | HTTP 处理：认证、目录树、转发、重定向跟随、错误映射、下载代理协议的调用方 |
| `docs/proxy-protocol.md` | 下载代理协议的定义 |
| `Dockerfile`、`.dockerignore` | 多架构镜像构建，见“使用 Docker” |
