# 一键迁移:sub2api + resin

把现在散在两台机器上的四个组件搬到一台新机(目标:4v8g,法国)。

```
现在:  sub2api + postgres + redis  在腾讯云(国内)
       resin                       在 45.205.28.160(海外)
       两者之间跨境 ~150ms

之后:  四个组件全在同一台机器的同一个 compose 网络里
```

---

## 先看清这次迁移的取舍

**跨境那一跳不会消失,它会换个位置。**

```
迁移前:  客户端(国内) → sub2api(腾讯云, ~20ms) →跨境 ~150ms→ resin(海外) → 节点 → 上游
迁移后:  客户端(国内) →跨境 ~200-300ms→ sub2api(法国) →容器网络 <1ms→ resin → 节点 → 上游
```

| | 变好 | 变差 |
|---|---|---|
| sub2api→resin 建连 | 跨境消失。这一跳原本在**每次**上游请求的关键路径上,resin 每换一个节点还要重付一次(最坏 3 次) | — |
| resin→节点→上游 | 法国离欧美出口节点近得多,长尾会明显收窄(97 秒那种) | — |
| 客户端→sub2api | — | 从 ~20ms 变成 ~200-300ms,**流式首字会慢约 200ms** |

**结论:交互式首字略慢,长请求与稳定性明显变好。**

如果首字延迟是你最在意的指标,还有一个选项:**只把 resin 搬到法国,sub2api 留在国内**。那样客户端仍然就近接入,而 sub2api→resin 那一跳仍旧跨境 —— 也就是现在的拓扑,只是 resin 换了个更好的落脚点。当前用的正是这种拓扑,所以那条路已经验证过可行。要走这条,只跑 resin 那一半即可(见下面「只迁 resin」)。

---

## 完整迁移(四件套全搬)

### 1. 构建镜像

**推荐在目标机上原生构建**,而不是本机交叉构建:

```bash
# 本机：只传源码（git archive，干净且小 —— 实测 sub2api 15MB / resin 3.5MB）
cd /path/to/sub2api && git archive --format=tar HEAD | gzip -1 > /tmp/sub2api-src.tar.gz
cd /path/to/Resin   && git archive --format=tar HEAD | gzip -1 > /tmp/resin-src.tar.gz
scp /tmp/{sub2api,resin}-src.tar.gz 新机:/opt/sub2api-stack/build/

# 目标机：原生 amd64 构建
cd /opt/sub2api-stack/build
tar -C sub2api -xzf sub2api-src.tar.gz && tar -C resin -xzf resin-src.tar.gz
docker build -t sub2api-stack/resin:local   resin/
docker build -t sub2api-stack/sub2api:local -f sub2api/deploy/Dockerfile sub2api/
```

> **为什么不在 Apple Silicon 上交叉构建。** 实测 `--platform linux/amd64` 会让**所有**
> stage 都跑在 QEMU 模拟下,包括前端那一层;Vite 构建在模拟环境里直接 OOM
> (`exit code 134` = SIGABRT,Node 堆溢出),即使 Dockerfile 里已经设了
> `--max-old-space-size=1536`。
>
> 而且交叉编译前端**本来就是浪费** —— 前端产物是一堆架构无关的静态文件,
> 让它在模拟器里跑纯属白费。目标机是原生 amd64、6.6G 可用内存,网络到 npm 只要
> 0.27s(法国离得近),在那里构建又快又省事。
>
> `build.sh` 保留了本机构建 + `docker save` 那条路(不依赖 registry),但它只适合
> 本机本来就是 amd64 的情况。

> **必须用我们自己构建的 resin 镜像。** 上游 `ghcr.io/resinat/resin` 不含二开改动
> (延迟天花板熔断、每 IP 租约上限、单请求内换节点重试、失格租约回收)。用错镜像的
> 症状很隐蔽:服务照常跑,但坏节点又开始把账号钉住几十秒。

> 在 Apple Silicon 上构建时 `build.sh` 已强制 `--platform linux/amd64`。
> 不指定的话会产出 arm64 镜像,它在目标机上**能 load、不能跑**,报 `exec format error`。

### 2. 旧机:导出数据

```bash
SUB2API_HOST=agcn RESIN_HOST=loon-resin bash export.sh
```

产出 `migrate-bundle-<时间戳>.tar.gz`,含:数据库全量、resin 状态目录、sub2api 生效的
`config.yaml`、resin 的两个 token、以及一份 `MANIFEST.txt`。

> `MANIFEST.txt` 不是装饰。迁移最常见的失败不是"没导出来",是**"导出来了但少了一半而没人发现"** ——
> 一个少了 100 个账号的数据库能完全正常地启动。导入侧会拿它逐项核对,对不上就拒绝继续。

> 导出的是 `/app/data/config.yaml`,**不是** `/etc/sub2api/config.yaml`。viper 的搜索
> 顺序是 `DATA_DIR → /app/data → . → ./config → /etc/sub2api`,前者会静默盖掉后者。
> 导错的话新环境会跑在一份看起来对、实际从没生效过的配置上。

### 3. 传输

```bash
scp migrate-bundle-*.tar.gz deploy/migrate/{docker-compose.yml,import.sh,verify-migration.sh,.env.example} 新机:/opt/sub2api-stack/
```

> 这个包里有明文凭据(数据库口令、resin token、账号 credentials)。走 scp,
> 不要经任何对象存储或聊天工具;导入完成后删掉。

> **部署目录不要用 `/opt/sub2api`。** 目标机上那个路径可能已经属于别的项目
> (实测 2026-09-16:目标机已有一套独立的 sub2api 部署)。用 `/opt/sub2api-stack`。

### 4. 新机:导入

```bash
cd /opt/sub2api-stack
bash import.sh migrate-bundle-*.tar.gz
```

### 5. 数据对照(切流量前必做)

```bash
OLD_SUB2API=agcn OLD_RESIN=loon-resin \
NEW_HOST='ssh -p 2222 -i ~/.ssh/新机密钥 root@新机IP' \
bash verify-migration.sh
```

逐项对照五类数据,退出码 0 才算等价。**`import.sh` 里的核对不够** —— 它只比行数,
而行数一致完全可能内容已经漂了。最容易漂的恰恰是配置:

- **sub2api 的配置有两个来源,DB 那份优先。** `config.yaml` 只是启动兜底,运维在
  管理后台改的每一项都落在 `settings` 表里(`setting_service.go:326` 让 DB 存值覆盖
  文件值)。只对照 config.yaml 会漏掉**全部** UI 改动。
- **resin 的平台配置不在文件里**,在它自己的 SQLite 里。订阅源白名单丢了不报错,
  只会让出口池悄悄退回"什么源都用"。
- **设备根指纹**:脚本对 `mirasim_device_seed` 做聚合哈希对照(不导出明文),
  证明是**同一批设备**而不只是"数量一样" —— 数量对而种子变了,上游看到的是一批全新设备。

---

## 这次迁移最容易出事的地方

**账号的出口代理地址存在数据库里**,现在指向旧部署的公网地址(`45.205.28.160:2260`)。
迁移后必须改成容器网络里的 `resin:2260`。`import.sh` 的第 5 步做这件事。

不做的后果特别有欺骗性:

- 服务全部正常启动,健康检查全绿,面板一切正常
- 但每个账号的请求都在往一台不该再用的旧机器上打
- 旧机器如果还活着 → **连报错都没有**,只是延迟莫名其妙地高
- 旧机器一关 → 全线崩掉,而此时你已经认为迁移成功好几天了

所以旧机的处理顺序是:**先停服务,别删**;新环境跑满一天再清理。

---

## 切流量之前必须确认的三件事

1. **出口真的走了新 resin** —— 随便挑一个账号点「查询」额度。秒回(< 2s)说明对了;
   要 60~90 秒说明第 5 步没生效,或 resin 状态目录没恢复。
2. **resin 的平台配置还在** —— 订阅源白名单(`regex_filters`)和 `region_filters`
   随 resin 状态目录一起过来。用 `deploy/apply-resin-runtime.sh` 回读校验一遍。
   丢了这个配置,出口池会退回"什么源都用",坏节点重新混进来。
3. **旧机先停服务别删**(见上)。

---

## 迁移后需要重新标定的东西

新机在法国,到各出口节点的延迟分布与现在完全不同,所以下面这些值是**基于旧拓扑测出来的**,
迁移后要重新看:

- `max_routable_latency_ms = 300` —— 法国到欧美节点更近,这个天花板可能可以收得更紧;
  但收之前先看实际分布,别照搬。
- **订阅源白名单** —— 四个源(radikal-fast / au1rxx / liangxin-paid / ldc兑换工艺)是按
  旧拓扑下的稳定性挑的。从法国出去的表现可能不同,值得重新观察一轮再决定要不要调整。
- `min_routable_throughput_bps` —— 吞吐熔断器一直没启用,因为缺实测依据定阈值。
  迁到低延迟环境后正好是采一批真实吞吐分布、把这个阈值定下来的好时机。

---

## 只迁 resin(保留现在的拓扑)

如果决定让 sub2api 留在国内:

```bash
# 新机
docker load -i images-*.tar.gz
docker compose up -d resin           # 只起 resin 这一个 service
```

然后把 resin 状态目录灌进去,并在 sub2api 侧把账号的代理地址改成新机的公网地址
(而不是 `resin:2260`)。注意此时 resin 的 2260 端口需要对 sub2api 所在 IP 开放 ——
compose 里默认只绑回环,要改成绑公网并**在云厂商安全组里只放通 sub2api 那一个 IP**。

---

## 资源配额的依据

配额不是拍的,是实测:

| 组件 | 实测 | 配额 | 说明 |
|---|---|---|---|
| resin | RSS 543 MB | 2 GB | 现在跑在 2核1G 上且 `available=0`,管着 4430 个节点 |
| sub2api | RSS 215 MB | 1 GB | |
| postgres | 23 MB 数据 | 1 GB | `accounts` 表 1.7 MB 是最大的 |
| redis | 极小 | 512 MB | `maxmemory 384mb` 配在 limit 之下,让它自己 LRU 淘汰而不是被 OOM killer 杀 |

合计 ~4.5 GB,4v8g 有富余。配额刻意留了倍数余量:resin 的内存随订阅源节点数增长,
而节点数会变。
