# FnCluster

FnCluster 把多台飞牛 fnOS 组成一个统一的环境：在一台机器上查看所有节点的状态、存储统计、应用目录，并能直接打开远端节点上的应用和整台桌面。每台节点仍然是独立的 fnOS 安装，集群只做管理与转发。

## 安装与升级

- 从 Releases 下载 `fncluster.fpk`（x86）或 `fncluster-arm.fpk`（arm），在飞牛「应用中心 → 手动安装」；升级用同一个入口。
- 夜构建在 Releases 的 `nightly` 预发布里，版本形如 `0.4.4-nightly.abc123`。
- 安装后桌面会出现 FnCluster 应用，打开即是管理页面。

## 开始使用

1. 在任意一台节点打开 FnCluster 应用。
2. 第一台：进入「集群 → 创建新集群」，选择本机地址并创建；再点「生成配对码」。
3. 其它节点：进入「集群 → 加入现有集群」，填写种子节点地址（IP:端口）和配对码，点「加入集群」。
4. 完成后到「总览 / 节点」查看状态。节点间连接立即生效，无需重启服务。

集群身份独立于飞牛账号，节点间通信默认使用 `38671` 端口与 mTLS。创建者可以移除成员，成员可以退出集群。

## 桌面集成

- 「桌面集成」页可以把 FnCluster 注入飞牛桌面：桌面角标显示在线节点数，全部应用里出现远端节点条目。
- 仅支持已验证的 fnOS 版本；未登记版本可用「兼容模式启用」。
- 启用会备份并修改飞牛桌面文件，健康检查失败自动回滚；「恢复原始桌面」随时还原。
- 角标可在页面上显示或隐藏。

## 远端应用与远端桌面

- 各节点发现的应用会以「应用名 · 节点名」出现在「全部应用」里，点击后通过集群中继打开，不需要在浏览器里直连远端。
- 每个在线节点还有「远端桌面 · 节点名」入口，可在窗口里使用设置、应用中心等完整桌面。
- 支持的应用形态：网关托管应用、Unix socket、CGI、自带回环端口的应用（含端口在运行设置里配置的应用）。

## 日志与恢复

- 页面「日志」可查看服务日志与审计日志。
- 恢复原始飞牛桌面（命令行，不依赖页面）：

```bash
/var/apps/fncluster/target/server/fncluster recover webui
```

- 补丁状态：`/var/apps/fncluster/target/server/fncluster webui status`。

## 从源码构建

```bash
go build -o fncluster ./cmd/fncluster

# Windows 上打包 FPK（需要 fnpack，可在 CI 使用的官方 Windows 版）
./packaging/fnos/build.ps1 -Arch amd64
./packaging/fnos/build.ps1 -Arch arm64
./packaging/fnos/build.ps1 -Arch amd64 -FnpackPath /path/to/fnpack.exe
```

## 安全边界

- 网页接口信任飞牛网关登录；写操作要求 `X-FnCluster: 1` 请求头并写入审计日志，没有独立管理密码，也没有任意 shell 接口。
- 节点间使用集群 mTLS；中继只允许访问本机回环地址与已声明的应用端口。
- 集群存储只是各节点容量的汇总统计，不是共享存储。
