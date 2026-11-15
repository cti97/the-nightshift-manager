package provider

const (
	// bytesPerToken is the default heuristic's ratio. English prose runs a
	// little over four bytes per token for the families this project talks to;
	// the ceiling division errs high, and the budget reserves above the result.
	bytesPerToken = 4
	// messageOverhead is the framing a chat template adds per turn: the role
	// token and the separators around the content.
	messageOverhead = 4
	// toolOverhead is the framing around one tool definition, separate from the
	// bytes of its name, description and schema.
	toolOverhead = 8
)

// estimateTokens is the default input-token estimate for every provider. It is
// the only implementation of the heuristic: adapters call it, and a family with
// a real tokenizer overrides Provider.EstimateTokens rather than copying this.
//
// # What the number is for
//
// The budget channel (M5) reserves estimated prompt + MaxTokens, multiplied by a
// safety factor, *before* a request is sent, and settles against the provider's
// reported Usage afterwards. Most APIs only report usage after the fact, so the
// estimate is a reservation, not a measurement. An estimator that runs low turns
// a budget into a suggestion; one that runs high leaves capacity unused.
//
// # The contract
//
// The default is a text-length heuristic, additive by construction:
//
//   - System contributes ceil(len/4) tokens.
//   - Every message contributes 4 tokens of chat framing plus ceil(len/4) for
//     each of Role, Name, CallID and Content, and ceil(len/4) for each tool
//     call's ID, Name and Arguments. An assistant turn's calls are replayed on
//     every later request, so they are prompt input like any other text.
//   - Every tool definition contributes 8 tokens of framing plus ceil(len/4)
//     for each of Name, Description and Schema. A schema is what the model
//     reads before it can call the tool; it is prompt input.
//   - Model, MaxTokens and the transport/egress selectors (Reach,
//     RequireLocal, RequireHost) are not counted. The model name selects an endpoint,
//     MaxTokens caps the reply, and the reach fields are a policy declaration;
//     none is prompt text. A request carrying only those is not a sendable
//     completion request and estimates 0.
//
// Three invariants follow, and estimate_test.go holds the default to them; the
// per-adapter tests pin that each adapter returns it, and the conformance
// suite's check 4 keeps every adapter within ±40% of a word oracle on prose:
//
//   - Monotone under growth. Every contribution is non-negative and nothing is
//     rounded across fields, so appending a message or a tool, or growing any
//     single field, never lowers the estimate. The executor grows a
//     conversation by appending to the history it already sent; that is the
//     operation the budget relies on.
//   - A floor of 1. A request carrying any input field estimates at least one
//     token. The zero Request estimates 0. No input can produce a negative
//     count.
//   - Pure. The estimate depends only on the request; there is no cache or
//     conversation state that could make a rebuilt request differ from an
//     appended one.
//
// # Accuracy, and which way the error runs
//
// There is no tokenizer in this package and no dependency is allowed, so the
// default is calibrated on English prose at about four bytes per token. Against
// the conformance suite's independent word oracle (4/3 tokens per word) it errs
// HIGH at paragraph scale: measured 70 tokens for the one-paragraph fixture
// (oracle 60, +16.7%) and 265 for the four-paragraph one (oracle 240, +10.4%),
// and TestEstimateTokensPinsTheDocumentedProseRatios holds those figures. A
// one-line turn is roughly neutral instead — the 4-token framing offsets the
// byte ratio's overcount — so the high-bias claim is about prose chunks, not
// every request. Erring high on prose is the safe direction for a budget that
// reserves up front.
//
// It errs LOW wherever bytes-per-token is not about four:
//
//   - Tool schemas and dense JSON generally. Punctuation tokenizes richly, so a
//     schema's 4-bytes-per-token share is likely less than its real cost. Tool
//     schemas are the largest exposure in practice: they are big, they are
//     JSON, and they are the field an estimator is most likely to forget.
//   - Non-ASCII text. len counts bytes, and UTF-8 is three bytes per CJK rune at
//     roughly one to two tokens per rune, so CJK can be underestimated by about
//     half. Emoji (four bytes, often two to four tokens) are likewise low.
//   - High-entropy text: hex ids, base64, digests and URLs tokenize at roughly
//     two to three bytes per token, so the 4-byte rule undercounts them.
//
// Two cost classes are deliberately outside the contract, and a consumer must
// not read the enumeration above as covering them:
//
//   - Attachments and multimodal parts. Request has no image or file field, so
//     there is nothing to count today. If such a field is added, estimateTokens
//     must gain a term for it; otherwise that cost is silently free.
//   - Cache and prefix credits. The estimate is gross: a replayed prefix is
//     charged in full even when a provider would bill it cached. A discount
//     appears only when the budget settles against Usage; a reservation must
//     never depend on one.
//
// The 1.2x reserve absorbs a 20% miss, not a 2x one, so a schema-heavy or
// non-ASCII prompt can under-reserve. The post-settle re-check is what catches
// that, which is why it is not optional.
//
// # Overriding
//
// A family with a real tokenizer may implement Provider.EstimateTokens itself;
// that method is the only override seam. An override must keep the invariants
// above. Every adapter in this package currently returns this default: stub,
// openai, anthropic, and ollama (which embeds the openai adapter). A second
// heuristic would give the budget two numbers to disagree about for the same
// bytes, so a family should override only when it can be more precise, not
// merely different.
func estimateTokens(req Request) int {
	tokens := textTokens(req.System)
	for _, m := range req.Messages {
		tokens += messageOverhead
		tokens += textTokens(m.Role)
		tokens += textTokens(m.Name)
		tokens += textTokens(m.CallID)
		tokens += textTokens(m.Content)
		// An assistant turn's calls are sent back on every later request in the
		// conversation, so they are input like any other text: a large tool
		// argument must raise the estimate or the budget under-reserves the
		// step that replays it.
		for _, c := range m.ToolCalls {
			tokens += textTokens(c.ID)
			tokens += textTokens(c.Name)
			tokens += byteTokens(len(c.Arguments))
		}
	}
	for _, t := range req.Tools {
		tokens += toolOverhead
		tokens += textTokens(t.Name)
		tokens += textTokens(t.Description)
		tokens += byteTokens(len(t.Schema))
	}
	// The floor cannot fire today — a system prompt, a message or a tool each
	// already contributes at least one token — but it states the invariant any
	// future term must keep: input-bearing requests are positive, never
	// negative. It is written as "< 1" so a future subtraction cannot leak a
	// negative count into the budget.
	if tokens < 1 && (req.System != "" || len(req.Messages) > 0 || len(req.Tools) > 0) {
		return 1
	}
	return tokens
}

// byteTokens is the ceiling division for a field of n bytes.
func byteTokens(n int) int {
	if n == 0 {
		return 0
	}
	return (n + bytesPerToken - 1) / bytesPerToken
}

// textTokens is a ceiling division: an 8-byte string is two tokens, not one.
func textTokens(s string) int { return byteTokens(len(s)) }
