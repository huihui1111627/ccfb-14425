# aquaprofile — 水族馆维生系统持续性能剖析服务

用 Go 实现的持续剖析服务：收集几十个控制程序（水泵、过滤、温控、投料）上报的
调用栈采样，按版本对比哪里变慢，并提供火焰图界面。

## 快速开始

```bash
# 启动剖析服务（默认 :8080）
go run ./cmd/aquaprofiled -addr :8080

# 另开终端：生成 30 个控制程序 × 3 进程 × 2 版本的合成数据并上报
go run ./cmd/profgen -addr http://localhost:8080
# 只生成不上报，查看植入的放慢函数：
go run ./cmd/profgen --dry-run
```

浏览器打开 <http://localhost:8080/>，页面四个标签页：

1. **火焰图**：SVG 火焰图，点击节点放大，支持函数名高亮，下方列出符号表空隙/未知地址占比；
2. **版本差分火焰图**：旧/新版本归一化后的对齐调用树，红=变慢、绿=变快，附变化最大的函数表；
3. **热点函数排行**：按自身耗时排名，含自身/累计占比；
4. **上报情况**：每个进程的 PID、采样周期、样本数、模块数、时间范围和标签集合。

所有查询都支持时间段（`start_ns`/`end_ns`）和标签（`label.tank=T03`）筛选。

## 上报协议

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/modules` | POST | 上报某进程的模块装载布局（基址、大小、符号表） |
| `/api/samples` | POST | 上报采样批次，`period_ns` 是该进程每条样本代表的时间 |
| `/api/programs` | GET | 程序/版本目录 |
| `/api/flame` | GET | 合并后的调用树、函数表、空隙统计（折叠到 1/1000） |
| `/api/hot` | GET | 热点函数排行 |
| `/api/diff` | GET | 两版本归一化差分（树不折叠，差分内部独立折叠） |
| `/api/status` | GET | 上报情况总览 |

采样 JSON 示例：

```json
{
  "program": "pump-rack-A-00", "version": "v2.0", "pid": 1000,
  "period_ns": 5000000,
  "samples": [{
    "time_ns": 1700000000000000000,
    "addresses": [4198400, 4199200, 140737488356384],
    "labels": {"tank": "T03", "pump": "2"}
  }]
}
```

## 代码结构

```
internal/symbolize  地址 → 模块内偏移 → 函数帧；空隙/未知地址分类
internal/merge      前缀调用树合并、自身/累计耗时、递归去重、1/1000 折叠
internal/diff       按各自总样本数归一化的版本差分（整数有理数运算）
internal/store      上报存储、时间段/标签筛选、确定性查询
internal/server     HTTP API + 内嵌前端（internal/server/web）
internal/gen        合成舰队生成器（含植入放慢函数和符号表空隙）
cmd/aquaprofiled    服务入口
cmd/profgen         合成数据上报器
```

## 八项设计要求的实现位置

1. **同栈不同标签分开记**：标签随每条样本存储（`store.Sample.Labels`），
   查询时先按标签过滤再合并，标签不同的样本不会落到任何共享聚合里。
2. **前缀合并、自身/总耗时、递归不重复**：`merge.Merge` 沿帧序列走前缀树，
   每节点对每条样本只累计一次；函数维度用 per-sample 的 `seen` 集合去重，
   递归帧在同一样本里重复出现总耗时只算一次（`merge_test.go` 有专门断言）。
3. **模块内偏移解析**：符号地址先减模块基址转成偏移（`symbolize.NewResolver`），
   同一模块在各进程装载地址不同也能解析到同一函数；落在模块内但符号表
   空隙的地址归 `模块![gap]`，不在任何模块内的归 `[unknown]!0x…`。
4. **筛选后合并、结果确定**：所有输出切片都按名字排序，合并只依赖样本多重集，
   与上报顺序无关；`TestMergeOrderIndependence`、`TestShuffledIngestionGivesIdenticalAPIOutput`
   在随机打乱上报顺序后做逐字节 JSON 比对。
5. **差分按各自总样本数归一化**：`diff.Build` 用 `count*1e9/total` 的整数缩放
   交叉比较，无浮点误差；方向标 `slower/faster/same`，按变化绝对值排名。
6. **不同采样频率按时间加权**：每条样本权重 = 上报进程的 `period_ns`，
   合并累加权重而非样本个数（`TestDifferentWeightsNotRawCounts`）。
7. **兄弟按函数名排序、千分之一折叠**：树的每层兄弟按 `(函数名, 模块名)` 排序，
   两版本形状可对照；严格小于父节点宽度 1/1000 的子节点收进一个 `[folded]` 叶子，
   恰好等于 1/1000 的保留（阈值边界有测试）。
8. **空隙样本单独归类统计占比**：`Result.Gaps`/`Result.Unknowns` 按帧给出
   样本数、权重和占比，前端火焰图页底部有专门表格。

## 测试与对账

测试里有一套**完全独立的逐条暴力统计**（`internal/server/e2e_test.go` 中的
`bruteTree`/`bruteFunctions`/`bruteMergeHoles`），不复用生产合并代码，对 8~30 个
程序、多种标签和时间窗筛选条件，逐节点、逐函数、逐空隙帧与 HTTP API 输出对账：

- 调用树每个保留节点的自身/累计/计数与暴力统计完全一致，折叠只删节点不改数；
- 函数表（含递归去重）与暴力统计完全一致；
- 空隙/未知地址的样本数和权重与暴力统计完全一致；
- 差分测试断言：植入的 `ctrl.pump_control`、`ctrl.heater_pid`、`drv.can_send`
  排在差分最前面且标慢，`runtime.idle` 等量变快，**所有没动过的叶子函数
  方向必须是 same 且计数零变化**；
- 归一化用手工小数据（80/20 vs 60/140）核对有理数精确值。

```bash
go test ./...          # 全部测试
go test -race ./...    # 含竞态检测
go test -cover ./...   # 内部包覆盖率 86%~95%，server 93%
```

## 合成数据说明

`internal/gen` 生成确定性数据（固定种子）：

- 30 个控制程序，每个 3 个进程，进程采样周期分别为 5/10/20 ms；
- 每个进程两个版本的模块基址全部不同，验证偏移解析；
- 符号表含符号间空隙和模块尾部空洞，另有完全不在模块内的地址；
- v2 中把 `runtime.idle` 的样本整数划拨给三个植入函数，其余模板样本数
  逐进程保持完全相同，因此“未改动函数不被误标”可以被严格断言；
- 含一条递归栈（`runtime.mem_alloc` 出现两次）验证累计耗时不重复。
