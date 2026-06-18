package anthropic

import "goodkind.io/clyde/internal/slogger"

var anthropicRequestLog = slogger.Concern(slogger.ConcernAdapterProviderAnthReq)
