# 온톨로지·지식 그래프로 쓰기

BangDB에서 개념(클래스)·관계·인스턴스를 저장하고 "어떤 개념 아래에 무엇이 있나", "A와 B는 어떻게 이어지나" 같은 질문을 빠르게 하는 방법입니다. 별도 그래프 DB 없이 SQL 테이블 위에서 동작합니다.

| 기능 | 하는 일 | 쓰는 곳 |
|---|---|---|
| `CREATE CLOSURE` | 계층(상하위)의 모든 조상·자손 관계를 자동으로 유지하는 표 | subClassOf, 조직도, 부품 분류, 카테고리 |
| `graph_walk` / `graph_path` | 임의 관계를 따라 닿는 노드·최단 경로 | partOf, connectedTo, 트리플의 특정 술어 |
| 뷰·CTE 조건 push-down | 뷰나 `WITH`로 만든 질의에 준 조건을 안쪽 계산에 먼저 반영 | 재귀 CTE 뷰를 한 개념에 대해 조회 |
| 기존 기능 | 재귀 CTE, 트리거, CHECK/FK, 식 인덱스, JSON | 추론 규칙, 제약 검증, 속성 저장 |

## 1. 스키마 예

```sql
-- 개념(클래스)과 다중 상속을 허용하는 상하위 관계
CREATE TABLE class (iri TEXT PRIMARY KEY, label TEXT COLLATE NOCASE, comment TEXT);
CREATE TABLE subclass (child TEXT NOT NULL REFERENCES class (iri) ON DELETE CASCADE,
                       parent TEXT REFERENCES class (iri) ON DELETE CASCADE,
                       UNIQUE (child, parent));
CREATE INDEX class_label ON class (label);

-- 계층의 전이 폐포: 자동 유지
CREATE CLOSURE class_tree ON subclass (child, parent);

-- 일반 관계는 트리플로 (정수 id를 쓰면 저장 공간과 속도가 유리)
CREATE TABLE term (id INTEGER PRIMARY KEY, iri TEXT UNIQUE NOT NULL);
CREATE TABLE triple (s INT NOT NULL, p INT NOT NULL, o INT NOT NULL, PRIMARY KEY (s, p, o));
CREATE INDEX triple_pos ON triple (p, o, s);
CREATE INDEX triple_osp ON triple (o, s, p);
```

`PRIMARY KEY (s, p, o)`와 두 인덱스(POS, OSP)로 주어·술어·목적어 중 무엇이 정해져도 인덱스로 찾습니다. 세 인덱스 모두 covering이라 행을 읽지 않습니다.

## 2. 계층: `CREATE CLOSURE`

```sql
CREATE CLOSURE 이름 ON 원본테이블 (자식열, 부모열);
DROP CLOSURE [IF EXISTS] 이름;
```

`이름(anc, des, depth)` 테이블이 만들어지고, 원본의 간선(자식 → 부모)을 따라 닿는 모든 (조상, 자손) 쌍과 최단 거리가 들어 있습니다. 원본이 두 형태 모두 됩니다.

- **트리 테이블** `class (id, parent)`: 루트는 `parent`가 NULL
- **간선 테이블** `subclass (child, parent)`: 한 자식에 여러 행(다중 상속, DAG). 루트도 `(루트, NULL)` 행을 두면 자기 자신 행(depth 0)이 생깁니다

```sql
SELECT des FROM class_tree WHERE anc = 'ex:Fastener';           -- 하위 전체 (자신 포함)
SELECT anc, depth FROM class_tree WHERE des = 'ex:HexBolt'
  ORDER BY depth;                                                -- 상위 전체, 가까운 순
SELECT 1 FROM class_tree WHERE anc = 'ex:Part' AND des = ?;      -- "?는 Part의 일종인가"

-- 인스턴스 분류: 어떤 개념(또는 그 하위)의 인스턴스 전부
SELECT i.* FROM instance i JOIN class_tree t ON t.des = i.type WHERE t.anc = 'ex:Fastener';
```

동작 방식:

- 원본에 대한 INSERT·DELETE·자식/부모 열의 UPDATE와 외래 키 연쇄 동작을 행 단위로 기록했다가 **문장이 끝날 때** 바뀐 자식과 그 아래 노드만 위에서부터 다시 계산합니다. 달라진 행만 고치므로 하위 트리를 옮겨도 그 안쪽 관계는 그대로 둡니다.
- 라벨처럼 자식·부모 열이 아닌 열의 UPDATE는 비용이 없습니다.
- **순환을 만드는 간선은 문장 전체를 실패시킵니다**(아무것도 바뀌지 않음).
- 폐포 테이블은 읽기 전용입니다. 인덱스는 추가할 수 있습니다. 원본 테이블·열 이름 변경은 따라가고, 쓰이는 열 삭제와 원본 삭제는 거부합니다(`DROP CLOSURE` 먼저).
- 재오픈·`VACUUM`·백업·복제에 그대로 포함되고, 덤프에는 `CREATE CLOSURE` 문장으로 나가 가져올 때 다시 만들어집니다. `IntegrityCheck`가 원본으로 독립 계산한 결과와 비교합니다.
- 원본과 같은 저장 방식을 씁니다(`USING memory` 원본이면 폐포도 메모리). 시계열 테이블은 원본이 될 수 없습니다.
- 자식 열에 인덱스가 없으면 `<이름>_<자식열>` 인덱스를 원본에 만들고, `DROP CLOSURE` 때 지웁니다.
- 문장이 실행되는 동안(그 문장이 일으킨 트리거 안 포함)은 문장 시작 시점의 폐포가 보입니다.

측정(5만 개 무작위 트리, 메모리 DB, 4코어):

| 작업 | 시간 |
|---|---|
| `CREATE CLOSURE` (폐포 59만 행) | 약 4 s |
| 한 개념의 하위 전체(3,823개) 조회 | 1 ms |
| 잎 개념 추가 | 0.3 ms |
| 라벨 변경 | 0.06 ms |
| 3,823개 하위 트리 이동 | 0.33 s |

## 3. 일반 관계: `graph_walk`, `graph_path`

```sql
graph_walk(간선테이블, 출발열, 도착열, 시작 [, 최대깊이 [, 방향]])  -- 열: node, depth, parent
graph_path(간선테이블, 출발열, 도착열, 시작, 목표 [, 최대깊이 [, 방향]])  -- 열: step, node
```

- 간선 = 한 행의 `출발열` 값 → `도착열` 값. 방향은 `'out'`(기본), `'in'`(거꾸로), `'both'`.
- `graph_walk`는 닿는 노드마다 한 행: 최단 거리(`depth`)와 최단 경로상의 직전 노드(`parent`). 시작 노드 자신은 depth 0, parent NULL. 너비 우선 순서로 내보내므로 `LIMIT`이면 그만큼만 탐색합니다.
- `graph_path`는 최단 경로 하나를 시작부터 목표까지(`step` 0, 1, …). 닿지 않으면 행이 없습니다.
- 순환이 있어도 각 노드를 한 번만 방문합니다. `최대깊이` NULL은 제한 없음.
- 간선 테이블은 조회 방향의 열(out이면 출발열, in이면 도착열)에 인덱스나 rowid가 있으면 노드마다 인덱스로 찾고, 없으면 문장마다 한 번 전체를 읽어 메모리에 둡니다.
- **조건이 있는 관계는 뷰로**: 뷰를 이름으로 주면 문장마다 한 번 계산해 씁니다.

```sql
-- 트리플에서 술어 하나(partOf = 7)만 따라가기
CREATE VIEW part_of AS SELECT s, o FROM triple WHERE p = 7;
SELECT node, depth FROM graph_walk('part_of', 's', 'o', :bolt);          -- 볼트가 속한 상위 조립품
SELECT node FROM graph_walk('part_of', 's', 'o', :pump, NULL, 'in');     -- 펌프를 이루는 부품 전부
SELECT t.iri FROM graph_path('part_of', 's', 'o', :bolt, :plant) g
  JOIN term t ON t.id = g.node ORDER BY g.step;                          -- 볼트 → 공장 경로

-- 왼쪽 테이블의 값을 시작점으로 (행마다 탐색)
SELECT c.label, w.node FROM class c, graph_walk('part_of', 's', 'o', c.id, 1) w WHERE w.depth = 1;
```

측정: 트리플 10만 개(노드 2만, 술어 20개, 인덱스 있는 테이블)에서 모든 술어로 3단계 탐색(155개 노드) 0.7 ms, 술어 하나의 뷰로 6단계 탐색 4 ms(뷰 계산 포함).

## 4. 재귀 CTE와 조건 push-down

폐포 테이블 없이 재귀 CTE로 계층을 뷰로 정의해도 됩니다. 바깥 질의의 `열 = 상수`, `열 IN (상수…)` 조건은 뷰·CTE·파생 테이블 안으로 먼저 들어갑니다.

```sql
CREATE VIEW subclass_closure AS
  WITH RECURSIVE r (anc, des) AS (
    SELECT id, id FROM class
    UNION ALL
    SELECT r.anc, c.id FROM class c JOIN r ON c.parent = r.des)
  SELECT anc, des FROM r;

SELECT des FROM subclass_closure WHERE anc = 7;   -- 7번 아래만 계산
EXPLAIN SELECT des FROM subclass_closure WHERE anc = 7;
-- SCAN SUBQUERY subclass_closure (pushed anc=?)
```

- 일반 SELECT·UNION 등은 해당 열의 식으로 바꿔 각 SELECT의 WHERE(그룹이면 HAVING)에 넣습니다.
- 재귀 CTE는 **반복 단계가 그 열을 그대로 넘기는 경우**(위의 `r.anc`)에만 시작 SELECT에 넣습니다. 반복할 때마다 바뀌는 열(`des`)의 조건은 넣을 수 없어 전체를 계산합니다. 이때는 반대 방향 뷰를 따로 만들거나 `CREATE CLOSURE`를 쓰세요.
- `LIMIT`/`OFFSET`, 윈도 함수, `*` 항목이 있는 본문, 다른 테이블 값이나 함수 호출로 정해지는 조건은 넣지 않습니다(결과가 바뀔 수 있으므로). 바깥 조건은 항상 다시 평가합니다.
- 10만 개 계층에서 `WHERE anc = 7` 조회가 2.6 s → 17 ms가 되었습니다.

## 5. 추론과 제약 (기존 기능으로)

- **RDFS 수준 추론**: 타입 추론(인스턴스 → 모든 상위 클래스)은 폐포 테이블과 조인 한 번, `rdfs:domain`/`range`는 뷰로 표현합니다.

  ```sql
  CREATE VIEW inferred_type AS
    SELECT i.id, t.anc AS type FROM instance i JOIN class_tree t ON t.des = i.type;
  ```
- **제약 검증(SHACL 수준)**: 값 범위·필수 속성은 `CHECK`·`NOT NULL`·`FOREIGN KEY`, 카디널리티는 `UNIQUE` 또는 위반을 보여 주는 뷰, 강제는 트리거 `RAISE(ABORT, …)`.
- **속성이 자주 바뀌는 개념**은 JSON 열 + 식 인덱스(`CREATE INDEX … (json_extract(props, '$.unit'))`).

## 6. 하지 않는 것과 남은 과제

- OWL DL 수준의 자동 추론기, SPARQL·Cypher 같은 별도 질의 언어는 없습니다(SQL로 표현). SPARQL 일부를 SQL로 바꾸는 변환기는 별도 패키지로 둘 수 있는 후보입니다.
- 라벨 검색은 `LIKE`(접두사 `'bolt%'`는 인덱스 사용)뿐이고 전문 검색 색인은 없습니다.
- 마주 보는 두 방향의 재귀 CTE 조건(위 4의 `des` 조건), 다른 테이블 값으로 정해지는 조건은 push-down되지 않습니다. 이 경우는 `graph_walk`의 인수에 왼쪽 테이블 값을 쓰면 됩니다.
- IRI·리터럴을 자동으로 정수 id로 바꾸는 트리플 전용 저장 방식은 없습니다(`term` 테이블로 직접).
