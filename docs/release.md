# 统一构建与第三方商店发布

五个 provider 的唯一 ID 来自根目录 `registry.json`。构建脚本和 GitHub Actions 都读取同一清单，避免漏发 CodeArts 或 ZCode。每个 provider 保持独立的 auth/executor identifier；CPA 当前的单 provider 动态库 ABI 决定了一个项目会生成五个动态库。

第三方源：

```text
https://raw.githubusercontent.com/zyxzjyzjj/cpa-multi-plugins/main/registry.json
```

该地址要在本次修改推送到 main 并完成 release 后才包含本次可安装版本。添加源不会自动发布本地代码。

## 本地构建

需要 Go 1.26、Python 3、C 编译器；面板测试需要 Node.js。

```bash
CC=gcc bash scripts/build.sh
python scripts/package-release.py 0.12.89 windows amd64
python -m unittest discover -s scripts -p 'test_*.py'
node --test plugins/codearts-provider/tools/panel-regression.test.cjs
```

`CC` 可以指定 MinGW、Clang 或交叉编译器。macOS 使用 `CC=clang`；跨平台编译还需要对应平台的 C 工具链和 SDK。

输出位于 `release-assets/`：

- 每个 provider 一个 `<id>_0.12.89_<os>_<arch>.zip`，根目录仅包含 `<id>.dll` / `.so` / `.dylib`。
- `cpa-multi-plugins-<os>-<arch>.zip`：全部五个 provider，供手动安装。
- `checksums-<os>-<arch>.txt`：该平台所有包的 SHA-256 校验值；单平台发布时将其复制为 `checksums.txt`，多平台发布时合并各平台清单。

## GitHub Actions

1. 提交并推送源码到 main，普通 main 构建会测试和生成 artifacts，不发布 release。
2. 确认 `VERSION` 与 `registry.json` 各条目的版本一致。
3. 创建并推送对应 tag（本次为 `v0.12.89`），或手动运行 Build & Release Plugins 工作流并填写版本 tag。
4. 工作流构建 Linux amd64/arm64、Windows amd64、macOS arm64/amd64，上传每个 provider 的包及合并后的 `checksums.txt`。macOS amd64 保留原工作流的可选交叉构建策略。

商店依据 `repository` 查找 GitHub release，而非直接安装整个项目 zip。因此所有 registry 条目必须指向拥有这些安装包的仓库。Fork 后要同时更新条目的 repository、homepage 和商店源地址。

## CodeArts 迁移

替换原 CodeArts 动态库后仍使用 `codearts-provider`，配置和账号文件可继续使用。保留原实现 0.1.18 的完整协议和功能；本项目发行版本统一为 0.12.89。避免同时放置原插件和本项目同标识的 CodeArts 动态库。

## 本次本地验证

- 五个 Go module 的 `go test ./...` 均通过。
- CodeArts 16 项面板回归测试通过；浏览器验证浅色、纯白、深色与 390px 移动端，未发现脚本错误或横向溢出。
- 真实 CPA 测试实例加载全部五个 Windows 动态库，校验独立 OAuth provider、版本和仓库归属均正确。
- CodeArts 真实 CPA 宿主加本地模拟上游的完整功能集成测试通过。真实厂商服务和其他操作系统的动态库本次未实测，交由发布矩阵构建。

可复用宿主测试（使用临时账号目录和模拟凭证，不读取个人账号）：

```bash
CPA_HOST_EXE=/path/to/cpa CPA_BUNDLE_DIR=/path/to/dist/linux-amd64 \
  python -m unittest discover -s scripts -p 'test_*.py'
```
