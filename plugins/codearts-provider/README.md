# CodeArts provider

华为 CodeArts 插件，来自用户维护的 [cpa-codearts-plugin](https://github.com/zyxzjyzjj/cpa-codearts-plugin) 0.1.18，现按本仓库规范放入独立 Go module，参与统一构建、测试和商店发布。

保留 `codearts-provider` 作为插件、认证与执行器标识。旧插件的账号文件和配置可以继续使用；安装本版本时替换旧动态库，避免同时加载两个相同标识的插件。

支持浏览器登录、凭证刷新、按账号发现模型、OpenAI/Anthropic/Responses 请求、流式首包错误处理、配额查询、签到、定时领取及账号并发调度。配置详见 [config.example.yaml](config.example.yaml)。

完整功能包括 PKCE/DPoP 授权、AK/SK 导入、Agent/Native 双协议、工具调用、思考等级、token 计数与用量统计、模型别名、福利模型显式刷新、订阅额度与福利余额分别缓存、领取开关与任务状态持久化、每账号并发限制及会话释放。完整原使用手册保存在 [原插件手册](docs/original-guide.md)，能力声明见 [能力清单](docs/capability-matrix.md)。原手册中的独立仓库构建/发布命令仅作历史参考，本项目请使用根目录统一脚本。

面板沿用本项目的卡片、按钮和字体，并同步 CPA 主界面的浅色、纯白、深色主题；独立打开时跟随系统主题。所有原有管理接口和页面操作保留。

```yaml
plugins:
  enabled: true
  dir: "./plugins"
  configs:
    codearts-provider:
      enabled: true
```

管理面板：`<CPA 地址>/v0/resource/plugins/codearts-provider/panel`。

在仓库根目录运行 `bash scripts/build.sh` 构建全部插件，商店安装与发布参见 [主 README](../../README.md)。单独构建：

```bash
cd plugins/codearts-provider
CGO_ENABLED=1 go build -buildmode=c-shared -o codearts-provider.so .
go test ./...
node --test tools/panel-regression.test.cjs
```

`cpa_integration_test.go` 的真实 CPA 宿主测试需要设置 `CODEARTS_CPA_EXE` 和 `CODEARTS_PLUGIN_DLL`；没有设置时跳过，其余测试使用本地模拟上游，不需要真实账号。
