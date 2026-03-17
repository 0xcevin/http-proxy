# HTTP 用户可控代理服务器

一个功能完整的 Go HTTP/HTTPS 代理服务器，支持用户认证、访问控制和频次限制。

## 功能特性

### 🔐 认证系统
- **Basic Auth**: 用户名/密码认证
- **API Token**: 支持通过 `X-API-Token` Header 认证
- 可配置是否启用认证

### 🚦 访问控制
- **全局白名单**: 限制所有用户只能访问特定目标
- **全局黑名单**: 阻止访问敏感目标（默认阻止内网地址）
- **用户级别规则**:
  - 每个用户独立的允许/阻止列表
  - 支持通配符匹配 (`*.google.com`)
  - 优先匹配黑名单，其次白名单

### ⏱️ 频次限制
- **每秒限制**: 基于令牌桶算法的 QPS 限制
- **突发容量**: 允许短时间的突发请求
- **每日限额**: 每个用户每日总请求数限制

### 📊 日志记录
- 支持 JSON 和文本格式
- 记录调用方、目标方、状态码、传输字节数
- 可配置输出到文件或标准输出

## 快速开始

### 1. 安装依赖
```bash
cd /tmp/http-proxy
go mod tidy
```

### 2. 运行代理
```bash
# 使用默认配置
go run .

# 使用配置文件
go run . -config config.json
```

### 3. 测试代理
```bash
# 使用 Basic Auth
curl -x http://admin:admin123@localhost:8080 \
     https://api.github.com/user

# 使用 API Token
curl -x http://localhost:8080 \
     -H "X-API-Token: sk-live-abc123" \
     https://api.github.com/user

# 配置系统代理
export HTTP_PROXY=http://admin:admin123@localhost:8080
export HTTPS_PROXY=http://admin:admin123@localhost:8080
curl https://api.github.com/user
```

## 配置说明

### 配置文件结构

```json
{
  "listen_addr": ":8080",           // 监听地址
  "auth": {
    "enabled": true,                // 是否启用认证
    "users": {                      // Basic Auth 用户
      "admin": "admin123",
      "user1": "password1"
    },
    "api_tokens": {                 // API Token 映射
      "token-abc": "user1"
    }
  },
  "rules": {
    "allowed_targets": [],          // 全局白名单
    "blocked_targets": [            // 全局黑名单
      "localhost",
      "127.0.0.1",
      "10.0.0.0/8"
    ],
    "user_rules": {                 // 用户规则
      "user1": {
        "allowed_targets": ["*.google.com"],
        "blocked_targets": ["internal.com"],
        "rate_limit": {
          "requests_per_second": 10,
          "burst_size": 20,
          "daily_limit": 1000
        }
      }
    }
  },
  "logging": {
    "enabled": true,
    "format": "json",               // 或 "text"
    "file_path": "proxy.log"        // 空表示 stdout
  }
}
```

### 匹配规则

目标地址匹配支持：
- **精确匹配**: `github.com`
- **通配符匹配**: `*.google.com` 匹配 `mail.google.com`, `drive.google.com`
- **子串匹配**: `google` 匹配任何包含 google 的域名

### 频次限制算法

使用 [令牌桶算法](https://pkg.go.dev/golang.org/x/time/rate) 实现：
- `requests_per_second`: 每秒产生的令牌数（平均速率）
- `burst_size`: 桶的最大容量（突发请求上限）
- `daily_limit`: 每日最大请求数，每天 00:00 重置

## 使用场景

### 场景 1: API 网关代理
限制内部服务只能访问特定的外部 API：
```json
{
  "user_rules": {
    "api-service": {
      "allowed_targets": [
        "api.openai.com",
        "api.anthropic.com",
        "api.github.com"
      ],
      "rate_limit": {
        "requests_per_second": 50,
        "daily_limit": 100000
      }
    }
  }
}
```

### 场景 2: 开发环境代理
为不同开发人员分配不同权限：
```json
{
  "user_rules": {
    "senior-dev": {
      "allowed_targets": [],  // 允许所有
      "rate_limit": { "requests_per_second": 100 }
    },
    "junior-dev": {
      "allowed_targets": ["*.stackoverflow.com", "*.github.com"],
      "rate_limit": { "requests_per_second": 10, "daily_limit": 1000 }
    }
  }
}
```

### 场景 3: 只读代理
仅允许访问文档和知识库：
```json
{
  "user_rules": {
    "readonly": {
      "allowed_targets": [
        "*.wikipedia.org",
        "docs.python.org",
        "developer.mozilla.org"
      ],
      "blocked_targets": ["*"],
      "rate_limit": { "requests_per_second": 2 }
    }
  }
}
```

## 安全建议

1. **修改默认密码**: 生产环境务必更改默认用户名密码
2. **使用 TLS**: 在代理前端配置 HTTPS/TLS 终止
3. **限制内网访问**: 默认配置已阻止 RFC1918 内网地址
4. **日志审计**: 启用 JSON 格式日志便于审计和分析
5. **定期轮换 Token**: API Token 应定期更换

## 扩展开发

### 添加数据库支持
可以修改 `config.go` 从数据库加载用户规则：
```go
func LoadConfigFromDB(db *sql.DB) (*Config, error) {
    // 查询用户和规则
}
```

### 添加动态配置
通过管理 API 实时更新规则：
```go
http.HandleFunc("/admin/rules", handleUpdateRules)
```

### 添加流量统计
记录每个用户的带宽使用情况：
```go
type TrafficStats struct {
    User        string
    BytesIn     int64
    BytesOut    int64
    RequestCount int64
}
```

## License

MIT
