# 接口使用说明

服务地址以下用 `http://localhost:8080` 表示。对外部署时，在设置中填写客户端可访问的公开地址，生成图片链接会使用该地址。图片被清理后，对应链接将失效。

## 认证与模型

在管理后台的设置页面创建访问密钥。OpenAI 兼容接口使用：

```http
Authorization: Bearer 你的访问密钥
```

Gemini 兼容接口使用 `x-goog-api-key`，也可通过查询参数 `key` 传入访问密钥。密钥属于本服务，与 Adobe 登录凭据分开管理。

通过 `GET /v1/models` 查看 OpenAI 兼容模型目录。Gemini 客户端使用 `GET /v1beta/models`。请求中的模型名称应取自服务返回的目录；实际可用模型、比例、分辨率以账号可访问的 Adobe 模型为准。

当前香蕉模型为 Nano Banana 2（`gemini-3.1-flash-image-preview`）和 Nano Banana Pro（`gemini-3-pro-image-preview`）。

## Adobe 登录续期

在账号池直接粘贴 Cookie 即可导入，无需手动填写访问令牌、用户标识或 ARP。服务从 Cookie 恢复真实身份，更新 Cookie 和令牌。直接续期保留同账号已有风险会话；从登录浏览器同步时使用该页面的最新会话，具体见[账号使用说明](accounts.md)。

缺少风险会话时，服务尝试从浏览器补齐，再用 Cookie 更新最终访问令牌。凭据读取成功显示为“待验证”，只有真实生成成功才显示“可用”。实测新风险会话仍可能返回 `408 timeout_error / system under load`；这类响应不等同于 Cookie 失效，也不能据此确定是上游负载或风险校验。Adobe 要求重新验证时，在登录页面完成验证后重新同步。

普通上游请求支持带认证的代理，但浏览器续期暂不支持代理用户名密码。需要浏览器同步时，应配置不带账号密码的本地或上游代理出口。

## OpenAI 图片生成

`POST /v1/images/generations`，请求类型为 `application/json`。

```json
{
  "model": "gemini-3.1-flash-image-preview",
  "prompt": "白色背景中的一只橘猫，柔和自然光",
  "n": 1,
  "size": "1024x1024",
  "response_format": "url"
}
```

| 字段 | 用途 |
| --- | --- |
| `model` | 服务模型目录中的名称 |
| `prompt` | 必填的图片描述 |
| `n` | 生成数量，支持 1 到 4，省略时为 1 |
| `size` | `宽x高`，或 `auto` |
| `aspect_ratio` | 扩展字段，用于直接指定比例，例如 `16:9` |
| `resolution` | 扩展字段，用于指定模型支持的分辨率档位，例如 `1K`、`2K`、`4K` |
| `quality` | 模型支持的画质档位 |
| `response_format` | `url` 或 `b64_json` |

`size`、`aspect_ratio` 与 `resolution` 必须一致，不支持的组合会返回参数错误。香蕉将尺寸转换为分辨率档位，比例单独传递；GPT Image 2、Sunburst、Flare 的明确 `size` 经校验后原样提交。

GPT Image 2、Sunburst 和 Flare 支持 1K、2K、4K。GPT 最长边为 3840 像素、总像素上限为 8,294,400；4K 方形为 2880×2880，3:2 为 3504×2336，GPT Image 2 的 16:9 为 3840×2160。2K、4K 需指定画面比例；1K 可使用自动比例。香蕉 4K 的 4096×4096 档位与 GPT 的尺寸约束不同。GPT Image 1.5 暂保留 1K。

例如，生成 GPT Image 2 的 4K 横图：

```json
{
  "model": "gpt-image-2",
  "prompt": "清晨阳光照进白色房间，桌上一只陶瓷花瓶",
  "aspect_ratio": "16:9",
  "resolution": "4K",
  "response_format": "url"
}
```

URL 返回示例：

```json
{
  "created": 1791936000,
  "data": [
    {
      "url": "http://localhost:8080/images/图片标识"
    }
  ]
}
```

使用 `"response_format":"b64_json"` 时，`data` 中返回 `b64_json`，内容为图片的 Base64 编码，不含 `data:image/...;base64,` 前缀。

当前 OpenAI 官方 GPT Image 接口固定返回 Base64；本服务保留 `response_format=url`，以兼容使用 URL 的客户端。该选项属于本服务扩展，不表示 Adobe 上游实现了 OpenAI 的全部参数。

## OpenAI 参考图与编辑

`POST /v1/images/edits` 支持 `multipart/form-data`。文件字段使用 `image` 或 `image[]`，多个参考图重复提交同名字段。

```bash
curl http://localhost:8080/v1/images/edits \
  -H "Authorization: Bearer 你的访问密钥" \
  -F "model=gemini-3.1-flash-image-preview" \
  -F "prompt=保留主体，把背景改成雪山" \
  -F "image[]=@reference.png" \
  -F "response_format=url"
```

也支持 JSON 中的 `images[].image_url`，便于使用公开图片 URL 或 Data URL 的客户端调用：

```json
{
  "model": "gemini-3.1-flash-image-preview",
  "prompt": "保留主体，把背景改成雪山",
  "images": [
    {
      "image_url": "data:image/png;base64,参考图编码"
    }
  ],
  "response_format": "url"
}
```

图片数量、大小和格式受所选 Adobe 模型限制。参考图用于模型重绘与组合；不支持的遮罩、文件编号等能力应返回参数错误，不能当作已执行。

## OpenAI 聊天入口

`POST /v1/chat/completions` 将聊天消息转换为生图提示。此入口面向通过聊天协议发起图片生成的客户端。

```json
{
  "model": "gemini-3.1-flash-image-preview",
  "messages": [
    {
      "role": "user",
      "content": [
        {
          "type": "text",
          "text": "参考这张照片，生成水彩风格头像"
        },
        {
          "type": "image_url",
          "image_url": {
            "url": "data:image/jpeg;base64,参考图编码"
          }
        }
      ]
    }
  ],
  "stream": false
}
```

返回 `chat.completion`，助手消息中的 Markdown 图片链接可由聊天客户端展示。该图片表达方式属于兼容扩展；OpenAI 官方 Chat Completions 的图片输入格式不等于官方提供此类生图输出。

```json
{
  "id": "chatcmpl-请求标识",
  "object": "chat.completion",
  "created": 1791936000,
  "model": "请求的模型",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "![生成图片](http://localhost:8080/images/图片标识)"
      },
      "finish_reason": "stop"
    }
  ]
}
```

设置 `stream:true` 后，使用 SSE 返回 `chat.completion.chunk`。完成帧包含 `finish_reason:"stop"`，随后返回 `data: [DONE]`。Adobe 生图完成之前不伪造部分图片。

## Gemini 图片生成与参考图

`POST /v1beta/models/{model}:generateContent`，请求类型为 `application/json`。

```json
{
  "contents": [
    {
      "role": "user",
      "parts": [
        {
          "text": "参考这张照片，生成水彩风格头像"
        },
        {
          "inlineData": {
            "mimeType": "image/png",
            "data": "参考图Base64编码"
          }
        }
      ]
    }
  ],
  "generationConfig": {
    "responseModalities": ["TEXT", "IMAGE"],
    "imageConfig": {
      "aspectRatio": "1:1",
      "imageSize": "1K"
    }
  }
}
```

纯文字生图时，删除参考图对应的 `inlineData` 部分。`inlineData.data` 使用普通 Base64，不使用 Data URL 前缀。图片 MIME 类型应与实际内容一致。

`generationConfig.imageConfig.aspectRatio` 和 `imageSize` 转换为当前 Adobe 模型支持的参数。Google 官方 ImageConfig 列出的完整范围，不代表每个 Adobe 模型都支持。

返回符合 `GenerateContentResponse` 的图片部分：

```json
{
  "candidates": [
    {
      "index": 0,
      "content": {
        "role": "model",
        "parts": [
          {
            "inlineData": {
              "mimeType": "image/png",
              "data": "生成图片Base64编码"
            }
          }
        ]
      },
      "finishReason": "STOP"
    }
  ],
  "modelVersion": "请求的模型"
}
```

Gemini 标准图片返回使用 `inlineData`。需要图片 URL 的客户端可使用 OpenAI 图片生成入口的 `response_format:url`。

## Gemini 流式入口

`POST /v1beta/models/{model}:streamGenerateContent?alt=sse` 使用相同请求结构。

响应类型为 `text/event-stream`，每个 `data:` 帧是一个 `GenerateContentResponse` 对象。最后一帧包含 `finishReason:"STOP"`，随后关闭响应。Gemini 流式协议不使用 OpenAI 的 `[DONE]` 结束标记。

流式传输只改变客户端接收方式；上游没有部分图片时，服务返回完成图片，不编造中间生成结果。

## 错误与记录

参数错误、认证失败、账号不可用和上游请求失败使用相应 HTTP 状态码。OpenAI 兼容入口采用 `error` 对象，Gemini 兼容入口采用包含 `code`、`message`、`status` 的 `error` 对象。客户端应先检查状态码，再处理成功响应。

Adobe 的积分消耗在管理后台日志中记录。服务查询 `https://firefly.adobe.io/v1/credits/balance`，在同一额度周期内核对生成前后的已使用量和可用余额；两者变化一致时记录积分消耗。额度重置、套餐变化、查询失败或数据矛盾时显示未知。账号登录状态刷新成功后，账号池显示查询到的可用余额；余额读取失败时显示未知。

积分不是 OpenAI 或 Gemini 的 token 数量，服务不凭空填写 `usage` 或 `usageMetadata`。服务内同一账号不会同时执行多个生成任务；若账号也在 Adobe 页面或其他程序中生成，前后余额的变化可能包含那些任务的消耗。

## 官方协议依据

以下页面于 2026 年 10 月 14 日核对。本服务的实际能力以当前模型目录和上游验证结果为准。

- [OpenAI 图片生成请求与响应](https://developers.openai.com/api/reference/resources/images/methods/generate)
- [OpenAI 图片编辑请求与响应](https://developers.openai.com/api/reference/resources/images/methods/edit)
- [OpenAI 图片生成指南](https://developers.openai.com/api/docs/guides/image-generation/)
- [OpenAI Chat Completions 请求格式](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
- [Gemini generateContent、streamGenerateContent、Content、Part 和 ImageConfig](https://ai.google.dev/api/generate-content)
- [Google 图片生成模型与使用指南](https://ai.google.dev/gemini-api/docs/image-generation)

Google 当前指南也介绍 Interactions API。本服务提供用户要求的 `generateContent` 兼容接口，不能把两种接口的请求字段混用。
