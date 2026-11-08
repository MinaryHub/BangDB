# BangDB 스크린샷

1920 × 1080 PNG, 앱 등록·소개용. 웹 콘솔(`bangdb -http`)과 CLI 셸을 v0.7로 캡처했습니다. 파일 이름 순서대로 보이며, 첫 장이 카드 대표 이미지가 됩니다.

| 파일 | 내용 |
|---|---|
| `01-query-join.png` | 주문·고객·상품 3개 테이블 조인 결과, 컬럼 타입 표시, 실행 시간 |
| `02-analytics-window.png` | 월별 카테고리 매출 집계와 `RANK() OVER` 윈도 함수 |
| `03-timeseries.png` | `USING timeseries` 테이블에서 `time_bucket`으로 서버별 1시간 평균 CPU |
| `04-explain-plan.png` | `EXPLAIN` — 복합 인덱스 범위 검색, 인덱스 순서로 정렬 생략, rowid 조인 |
| `05-dark-table-browse.png` | 다크 모드, 테이블 목록에서 `products`를 눌러 바로 조회 |
| `06-cli-shell.png` | CLI 셸 — `.tables`, `.timer`, 집계 쿼리, `.check` 무결성 검사 |
| `07-login.png` | 웹 콘솔 로그인 — 사용자/비밀번호, 토큰, 인증 없음 중 선택 |

데이터는 소규모 쇼핑몰 예제입니다: 고객 200명, 상품 16개, 주문 1,500건, 서버 지표 4,320개(시계열), 세션(`USING memory`), 뷰 `sales_by_category`. 이름·이메일은 모두 가상입니다.
