# jev-alert-demo

서버 21대(게임 20 + 배치 1) × 지표 6개의 "평소 값"과 "지금 값"을 가정해서 만들고,
같은 데이터를 세 방식으로 판정해 비교한다. 생성 방식과 Jev 방식은 같은 모델을 쓰고 사용 방법만 다르다.
파일 하나(`main.go`), Go 표준 라이브러리만 쓴다.

설명과 결과는 블로그 글 [알람 규칙 없이 LLM으로 알람 판정하기 - Jev 방식을 exaone으로 흉내 내기](https://corecode.pe.kr/normal/2026/09/27/jev-style-alert-with-exaone/)에 정리했다.

- 고정 알람 규칙: `CPU > 90%`, `에러율 > 2%`, `p99 > 500ms`
- LLM 판정(생성 방식): JSON으로 판정·원인·근거를 끝까지 생성
- LLM 판정(Jev 방식): 보기(A/B/C…)를 주고 `num_predict: 1`로 토큰 1개만 생성, 그 자리의 `logprobs`에서 보기별 확률을 읽음

## 실행

[Ollama](https://ollama.com)와 모델이 필요하다 (`/api/chat`의 `logprobs` 지원 버전, 0.34.4에서 확인).

```bash
ollama pull exaone3.5:2.4b-instruct-q4_K_M
go run .                    # 고정 규칙, Jev 방식
go run . -gen               # 생성 방식까지 (느림)
go run . -scenario mixed    # 시나리오 하나만: normal, deploy, mixed
```

## 결과 (M1 8GB, exaone3.5 2.4B)

칸의 값은 "알림 수 / 서버 21대 판정 시간"이다.

| 시나리오 | 고정 알람 규칙 | LLM 판정(생성 방식) | LLM 판정(Jev 방식) |
|---|---|---|---|
| 평상시 | 1 (배치 서버 오탐) | 0 / 40.4초 | 0 / 4.0초 |
| 배포 후 전체 에러율 상승 | 21 | 20 / 58.4초 | 1 (20대 묶음) / 17.0초 |
| CPU·동접·GC·메모리 동시 발생 | 2 (동접·GC·메모리 놓침) | 6 / 38.8초 | 6 / 6.3초 |

## 라이선스

MIT
