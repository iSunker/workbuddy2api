// check — 诊断工具：验证 CodeBuddy 凭证是否可用，并列出该凭证可见的模型。
//
// 用法：
//
//	go run ./cmd/check -key ck_xxxx                 # 探测 + 列模型
//	go run ./cmd/check -key ck_xxxx -model glm-5.2  # 额外跑一次真实对话
//	go run ./cmd/check -key ck_xxxx -env public     # 指定环境（internal/public/ioa）
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"codebuddy2api/internal/upstream"
)

func main() {
	key := flag.String("key", os.Getenv("CB2A_KEY"), "CodeBuddy API Key (ck_...) or Bearer token")
	base := flag.String("base", "", "upstream base URL (default: derived from -env)")
	env := flag.String("env", "internal", "internal | public | ioa")
	model := flag.String("model", "", "if set, run a real chat completion with this model")
	prompt := flag.String("prompt", "只回复 OK", "prompt used when -model is set")
	flag.Parse()

	token := strings.TrimSpace(*key)
	if token == "" {
		fmt.Fprintln(os.Stderr, "check: -key is required (or set CB2A_KEY)")
		os.Exit(2)
	}

	baseURL := *base
	if baseURL == "" {
		baseURL = baseForEnv(*env)
	}
	up := upstream.NewWithBase(baseURL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Printf("upstream : %s\n", up.Base)
	fmt.Printf("cred     : %s\n", mask(token))
	fmt.Println()

	// 1) 模型列表
	fmt.Print("models   : ")
	models, err := up.FetchModels(ctx, token)
	if err != nil {
		fmt.Printf("FAILED (%v)\n", err)
	} else {
		ids := make([]string, 0, len(models))
		for _, m := range models {
			ids = append(ids, m.ID)
		}
		fmt.Printf("OK (%d)\n", len(models))
		for _, id := range ids {
			fmt.Printf("           - %s\n", id)
		}
	}

	// 2) 连通性探测
	fmt.Print("probe    : ")
	if err := up.Probe(ctx, token); err != nil {
		fmt.Printf("FAILED (%v)\n", err)
	} else {
		fmt.Println("OK")
	}

	// 3) 可选：真实对话
	if *model != "" {
		fmt.Printf("chat     : model=%s ... ", *model)
		payload, _ := json.Marshal(map[string]any{
			"model":      *model,
			"messages":   []map[string]string{{"role": "user", "content": *prompt}},
			"stream":     true,
			"max_tokens": 64,
		})
		rc, err := up.ChatForward(ctx, token, payload)
		if err != nil {
			fmt.Printf("FAILED (%v)\n", err)
			os.Exit(1)
		}
		defer rc.Close()
		resp, err := upstream.Aggregate(rc, *model)
		if err != nil {
			fmt.Printf("FAILED (%v)\n", err)
			os.Exit(1)
		}
		raw, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Println("OK")
		fmt.Println()
		fmt.Println(string(raw))
	}
}

func baseForEnv(env string) string {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "ioa":
		return "https://tencent.sso.copilot.tencent.com"
	case "public", "overseas":
		return "https://www.codebuddy.ai"
	default:
		return "https://copilot.tencent.com"
	}
}

func mask(tok string) string {
	if len(tok) <= 12 {
		return "***"
	}
	return tok[:6] + "***" + tok[len(tok)-4:]
}
