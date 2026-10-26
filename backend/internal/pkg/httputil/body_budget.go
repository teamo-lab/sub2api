package httputil

import "errors"

// ErrRequestBodyBudgetExceeded 是读未知长度请求体时全局在途预算不够返回的读错误。
// 读请求体的 handler 应按服务端过载处理（503 + Retry-After），不是请求本身有问题。
var ErrRequestBodyBudgetExceeded = errors.New("request body budget exceeded")

// RequestBodyBudgetMessage 是预算不够时回给客户端的提示。
const RequestBodyBudgetMessage = "Server is busy processing other large requests, please retry later"

// RequestBodyBudgetRetryAfterSeconds 是预算不够时建议客户端等待的秒数。
const RequestBodyBudgetRetryAfterSeconds = 10
