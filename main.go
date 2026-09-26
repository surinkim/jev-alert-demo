// jev-alert-demo는 서버 지표를 가정해서 만들고, 같은 데이터를 세 가지 방식으로 판정해 비교한다.
//
//	고정 알람 규칙:       CPU > 90% 같은 임계값
//	LLM 판정(생성 방식): 판정·원인·근거를 JSON 문장으로 끝까지 생성
//	LLM 판정(Jev 방식):  정해 둔 보기 중 하나를 고르게 하고, 토큰 1개의 확률(logprob)만 읽음
//
// 생성 방식과 Jev 방식은 같은 모델을 쓴다. 다른 건 사용 방법뿐이다.
//
// 실행: go run . [-gen] [-scenario deploy] [-model exaone3.5:2.4b-instruct-q4_K_M]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// ─── 지표와 서버 ──────────────────────────────────────────────

type Metric struct {
	Name, Desc, Unit string
	MinDelta         float64 // "크게 바뀌었다"고 볼 최소 절대 변화량 (노이즈 거르기)
	Bad              int     // 나빠지는 방향: 1이면 오를 때, -1이면 내릴 때
}

var metrics = []Metric{
	{"cpu", "CPU 사용률", "%", 15, 1},
	{"mem", "메모리 사용률", "%", 15, 1},
	{"error_rate", "요청 에러율", "%", 0.5, 1},
	{"ccu", "동시 접속자", "명", 1000, -1},
	{"p99", "응답시간 p99", "ms", 50, 1},
	{"gc_pause", "GC 멈춤 시간", "ms", 50, 1},
}

// 평소 값. 배치 서버는 평소에도 CPU를 91% 쓴다.
var usual = map[string]map[string]float64{
	"game":  {"cpu": 35, "mem": 60, "error_rate": 0.1, "ccu": 5000, "p99": 100, "gc_pause": 10},
	"batch": {"cpu": 91, "mem": 70, "error_rate": 0.05, "ccu": 0, "p99": 50, "gc_pause": 12},
}

type Value struct{ Now, Usual float64 }

type Server struct {
	Name, Role string
	Values     map[string]Value
}

// fleet은 게임 서버 20대 + 배치 서버 1대를 만든다. 지금 값은 평소 값에 약간의 흔들림을 준다.
func fleet() []Server {
	var out []Server
	add := func(name, role string, i int) {
		s := Server{Name: name, Role: role, Values: map[string]Value{}}
		for j, m := range metrics {
			u := usual[role][m.Name]
			wobble := 1 + 0.04*math.Sin(float64(i*7+j*3)) // 결정적인 ±4% 흔들림
			s.Values[m.Name] = Value{Now: u * wobble, Usual: u}
		}
		out = append(out, s)
	}
	for i := 1; i <= 20; i++ {
		add(fmt.Sprintf("game-%02d", i), "game", i)
	}
	add("batch-01", "batch", 21)
	return out
}

// ─── 시나리오: 특정 서버의 특정 지표를 이 값으로 바꾼다 ─────────────

type Scenario struct {
	ID, Title string
	Apply     map[string]map[string]float64 // 서버 → 지표 → 지금 값 ("game-*"은 모든 게임 서버)
}

var scenarios = []Scenario{
	{"normal", "평상시", nil},
	{"deploy", "배포 후 모든 게임 서버의 에러율 상승", map[string]map[string]float64{
		"game-*": {"error_rate": 4},
	}},
	{"mixed", "서로 다른 서버에서 CPU·동접·GC·메모리 문제 동시 발생", map[string]map[string]float64{
		"game-07": {"cpu": 98},
		"game-03": {"ccu": 700}, "game-04": {"ccu": 700}, "game-05": {"ccu": 700},
		"game-12": {"gc_pause": 450, "p99": 190},
		"game-09": {"mem": 95},
	}},
}

func (sc Scenario) build() []Server {
	servers := fleet()
	for i, s := range servers {
		for target, vals := range sc.Apply {
			if target == s.Name || (target == "game-*" && s.Role == "game") {
				for m, v := range vals {
					servers[i].Values[m] = Value{Now: v, Usual: s.Values[m].Usual}
				}
			}
		}
	}
	return servers
}

// ─── 공통: 숫자 비교는 코드가 한다 ─────────────────────────────

// worse는 지표가 나쁜 방향으로 평소보다 50% 이상, 그리고 최소 변화량 이상 바뀌었는지 본다.
func worse(m Metric, v Value) bool {
	if v.Usual == 0 {
		return false
	}
	change := (v.Now - v.Usual) / v.Usual * 100
	return change*float64(m.Bad) >= 50 && math.Abs(v.Now-v.Usual) >= m.MinDelta
}

// 주의: 문구 한두 개로 결과가 크게 바뀐다. "평소와 비슷하다"를 "평소와 비슷하거나 더 좋다"로,
// 질문 끝에 "평소에도 높은 값이면 정상일 수 있다"를 붙이자 정상 서버의 "정상" 확률이 22%에서 90%가 됐다.

// fleetNotes는 게임 서버 절반 이상에서 같은 지표가 나빠졌는지 찾는다.
func fleetNotes(servers []Server) map[string]string {
	notes := map[string]string{}
	for _, m := range metrics {
		n, total := 0, 0
		for _, s := range servers {
			if s.Role == "game" {
				total++
				if worse(m, s.Values[m.Name]) {
					n++
				}
			}
		}
		if n*2 >= total && n >= 3 {
			notes[m.Name] = fmt.Sprintf("- 게임 서버 %d대 중 %d대에서 %s이(가) 동시에 크게 나빠졌다. 이 서버만의 문제가 아니다.", total, n, m.Desc)
		}
	}
	return notes
}

// describe는 LLM에게 줄 서버 설명이다. 생성 방식과 Jev 방식이 똑같이 쓴다.
// 작은 모델은 숫자 표에서 차이를 찾는 데 약해서, 나빠진 지표만 골라서 보여 준다.
func describe(s Server, notes map[string]string) string {
	var b, fleet strings.Builder
	fmt.Fprintf(&b, "서버: %s (%s)\n평소보다 크게 나빠진 지표:\n", s.Name, map[string]string{"game": "게임 서버", "batch": "배치 서버"}[s.Role])
	n := 0
	for _, m := range metrics {
		v := s.Values[m.Name]
		if !worse(m, v) {
			continue
		}
		fmt.Fprintf(&b, "- %s: 지금 %.1f%s, 평소 %.1f%s (%+.0f%%)\n", m.Desc, v.Now, m.Unit, v.Usual, m.Unit, (v.Now-v.Usual)/v.Usual*100)
		if note, ok := notes[m.Name]; ok {
			fleet.WriteString(note + "\n")
		}
		n++
	}
	if n == 0 {
		b.WriteString("- 없음. 모든 지표가 평소와 비슷하거나 더 좋다.\n")
	} else {
		b.WriteString("그 외 지표는 평소와 비슷하거나 더 좋다.\n")
	}
	if fleet.Len() > 0 {
		b.WriteString("같은 역할의 다른 서버들:\n" + fleet.String())
	}
	return b.String()
}

// ─── 고정 알람 규칙 ─────────────────────────────────────────────────

func judgeRules(s Server) string {
	switch {
	case s.Values["cpu"].Now > 90, s.Values["error_rate"].Now > 2:
		return "장애"
	case s.Values["p99"].Now > 500:
		return "주의"
	}
	return "정상" // 메모리, 동접, GC 규칙은 없다
}

// ─── Ollama 호출 ─────────────────────────────────────────────

var (
	ollamaURL = flag.String("ollama", "http://localhost:11434", "Ollama 주소")
	model     = flag.String("model", "exaone3.5:2.4b-instruct-q4_K_M", "모델")
)

type chatResponse struct {
	Message  struct{ Content string } `json:"message"`
	Logprobs []struct {
		TopLogprobs []struct {
			Token   string  `json:"token"`
			Logprob float64 `json:"logprob"`
		} `json:"top_logprobs"`
	} `json:"logprobs"`
	EvalCount int `json:"eval_count"` // 생성한 토큰 수
}

func chat(prompt string, extra map[string]any) (*chatResponse, error) {
	req := map[string]any{
		"model":    *model,
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	}
	for k, v := range extra {
		req[k] = v
	}
	body, _ := json.Marshal(req)
	resp, err := http.Post(*ollamaURL+"/api/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out chatResponse
	return &out, json.NewDecoder(resp.Body).Decode(&out)
}

// ─── LLM 판정(Jev 방식) ─────────────────────────────────────────────

var letters = []string{"A", "B", "C", "D", "E", "F", "G"}

// choose는 보기를 A), B), ...로 붙여 묻고 토큰을 1개만 생성시킨 뒤,
// 그 자리의 확률표(logprob)에서 보기 글자의 확률을 읽어 보기별 확률로 돌려준다.
func choose(question string, options []string, body string) ([]float64, error) {
	var p strings.Builder
	p.WriteString(question + "\n보기:\n") // 질문과 보기가 앞: 모든 서버에서 앞부분이 같아 Ollama가 재사용
	for i, o := range options {
		fmt.Fprintf(&p, "%s) %s\n", letters[i], o)
	}
	p.WriteString("\n" + body)
	// "답:"으로 끝내면 모델이 "답"부터 따라 써서 첫 토큰이 보기 글자가 아니게 된다.
	fmt.Fprintf(&p, "\n%s 중 알파벳 한 글자로만 답하라.", strings.Join(letters[:len(options)], ", "))

	resp, err := chat(p.String(), map[string]any{
		"logprobs":     true,                                               // 확률표를 달라
		"top_logprobs": 20,                                                 // 상위 후보 20개까지
		"options":      map[string]any{"num_predict": 1, "temperature": 0}, // 토큰 1개만 만들고 멈춤
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Logprobs) == 0 {
		return nil, fmt.Errorf("logprobs가 없음 (Ollama 버전 확인)")
	}

	// 확률표에서 보기 글자만 골라 e^logprob로 확률을 구하고, 합이 1이 되게 정규화한다.
	probs := make([]float64, len(options))
	var sum float64
	for _, t := range resp.Logprobs[0].TopLogprobs {
		for i := range options {
			if strings.TrimSpace(t.Token) == letters[i] && probs[i] == 0 {
				probs[i] = math.Exp(t.Logprob)
				sum += probs[i]
			}
		}
	}
	if sum == 0 {
		return nil, fmt.Errorf("확률표에 보기 글자가 없음: %q", resp.Message.Content)
	}
	for i := range probs {
		probs[i] /= sum
	}
	return probs, nil
}

const fleetCause = "전체 서버 공통 문제(배포 등)"

var (
	states = []string{"정상", "주의", "장애"}
	// 보기 순서가 답에 영향을 준다(위치 편향). 작은 모델로 시험해 보니 공통 원인을 맨 앞에 둘 때 가장 잘 구분했다.
	causes = []string{fleetCause, "CPU 과부하", "메모리 부족", "이 서버만의 에러 증가", "동시 접속자 급감", "응답 지연 또는 GC 멈춤", "알 수 없음"}
)

type jevResult struct {
	State, Cause         string
	StateProb, CauseProb float64
	Took                 time.Duration
}

func judgeJev(desc string) (jevResult, error) {
	start := time.Now()
	sp, err := choose("다음 서버의 상태를 보기에서 골라라. 평소 값과 비교해서 판단하라. 평소에도 높은 값이면 정상일 수 있다.", states, desc)
	if err != nil {
		return jevResult{}, err
	}
	r := jevResult{}
	r.State, r.StateProb = argmax(states, sp)
	if r.State != "정상" { // 정상이면 원인은 묻지 않는다
		cp, err := choose("다음 서버에 문제가 생긴 가장 가능성 높은 원인을 보기에서 골라라. "+
			"다른 서버들도 같은 지표가 동시에 나빠졌다면 전체 서버 공통 문제다.", causes, desc)
		if err != nil {
			return r, err
		}
		r.Cause, r.CauseProb = argmax(causes, cp)
	}
	r.Took = time.Since(start)
	return r, nil
}

func argmax(keys []string, ps []float64) (string, float64) {
	best := 0
	for i := range ps {
		if ps[i] > ps[best] {
			best = i
		}
	}
	return keys[best], ps[best]
}

// ─── LLM 판정(생성 방식) ──────────────────────────────────────

type genResult struct {
	Severity, Cause string
	Tokens          int
	Took            time.Duration
}

func judgeGen(desc string) (genResult, error) {
	start := time.Now()
	resp, err := chat(desc+"\n이 서버에 알림을 보내야 하는지 판단하라. "+
		`JSON으로만 답하라: {"severity": "정상|주의|장애", "cause": "원인 한 줄", "reason": "판단 근거"}`,
		map[string]any{"format": "json", "options": map[string]any{"num_predict": 300, "temperature": 0}})
	if err != nil {
		return genResult{}, err
	}
	var ans struct{ Severity, Cause string }
	if err := json.Unmarshal([]byte(resp.Message.Content), &ans); err != nil {
		return genResult{}, fmt.Errorf("JSON 해석 실패: %w", err)
	}
	return genResult{ans.Severity, ans.Cause, resp.EvalCount, time.Since(start)}, nil
}

// ─── 실행 ────────────────────────────────────────────────────

func main() {
	withGen := flag.Bool("gen", false, "LLM 판정(생성 방식)도 함께 비교 (느림)")
	only := flag.String("scenario", "", "이 시나리오만 실행 (normal, deploy, mixed)")
	flag.Parse()

	// 라벨 폭을 맞춘다 (터미널에서 한글은 두 칸)
	const (
		ruleLabel = "고정 규칙"
		genLabel  = "생성 방식"
		jevLabel  = "Jev 방식 "
	)

	for _, sc := range scenarios {
		if *only != "" && sc.ID != *only {
			continue
		}
		servers := sc.build()
		notes := fleetNotes(servers)
		fmt.Printf("\n━━ [%s] %s\n", sc.ID, sc.Title)

		var ruleAlerts, jevAlerts, genAlerts, grouped int
		var ruleTook, jevTook, genTook time.Duration
		for _, s := range servers {
			desc := describe(s, notes)

			start := time.Now()
			rule := judgeRules(s)
			ruleTook += time.Since(start)

			jev, err := judgeJev(desc)
			if err != nil {
				fmt.Println("Jev 방식 오류:", err)
				return
			}
			jevTook += jev.Took

			var genLine string
			genAlerting := false
			if *withGen {
				g, err := judgeGen(desc)
				if err != nil {
					genLine = "오류: " + err.Error()
				} else {
					genTook += g.Took
					genAlerting = g.Severity != "정상"
					genLine = fmt.Sprintf("%s · %s (%d토큰, %.1f초)", g.Severity, short(g.Cause, 20), g.Tokens, g.Took.Seconds())
				}
			}

			if rule != "정상" {
				ruleAlerts++
			}
			if genAlerting {
				genAlerts++
			}
			switch {
			case jev.Cause == fleetCause:
				grouped++
			case jev.State != "정상":
				jevAlerts++
			}

			// 세 방식 모두 정상인 서버는 생략한다
			if rule == "정상" && jev.State == "정상" && !genAlerting {
				continue
			}
			jevLine := fmt.Sprintf("%s %.0f%%", jev.State, jev.StateProb*100)
			if jev.Cause != "" {
				jevLine += fmt.Sprintf(" · %s %.0f%%", jev.Cause, jev.CauseProb*100)
			}
			jevLine += fmt.Sprintf(" (%.2f초)", jev.Took.Seconds())

			fmt.Printf("  %s\n", s.Name)
			fmt.Printf("    %s  %s\n", ruleLabel, rule)
			if *withGen {
				fmt.Printf("    %s  %s\n", genLabel, genLine)
			}
			fmt.Printf("    %s  %s\n", jevLabel, jevLine)
		}
		if grouped >= 3 {
			jevAlerts++ // 전체 공통 원인은 한 건으로 묶는다
		} else {
			jevAlerts += grouped
		}

		counts := []string{fmt.Sprintf("%s %d건", ruleLabel, ruleAlerts)}
		times := []string{fmt.Sprintf("%s %.1f초", ruleLabel, ruleTook.Seconds())}
		if *withGen {
			counts = append(counts, fmt.Sprintf("%s %d건", genLabel, genAlerts))
			times = append(times, fmt.Sprintf("%s %.1f초", genLabel, genTook.Seconds()))
		}
		jevCount := fmt.Sprintf("Jev 방식 %d건", jevAlerts)
		if grouped >= 3 {
			jevCount += fmt.Sprintf(" (%d대를 '전체 공통'으로 묶음)", grouped)
		}
		counts = append(counts, jevCount)
		times = append(times, fmt.Sprintf("Jev 방식 %.1f초", jevTook.Seconds()))

		fmt.Printf("  ── 알림 수     %s\n", strings.Join(counts, " · "))
		fmt.Printf("  ── %d대 판정   %s\n", len(servers), strings.Join(times, " · "))
	}
}

// short는 긴 문장을 n글자에서 자르고 "…"를 붙인다. 출력 한 줄이 너무 길어지지 않게 한다.
func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
