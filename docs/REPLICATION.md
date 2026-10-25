# 백업과 미러링 (스케일 아웃)

BangDB는 **온라인 백업**과 **primary → replica 미러링(물리 복제)**을 지원합니다. 새 replica는 항상 "백업(스냅샷) → 그 이후 커밋 스트리밍" 순서로 동기화되므로, 백업과 미러링은 같은 메커니즘 위에서 동작합니다.

```
                    쓰기
 앱 ──────────────▶ Primary (읽기/쓰기)
                     │  커밋마다 Frame(seq, 변경 페이지 이미지)
          ┌──────────┼──────────────┐
          ▼          ▼              ▼
      Replica 1   Replica 2      Replica N    ← 읽기 전용, 조회 분산 (read scale-out)
          ▲
 앱 ──────┘ 읽기
```

## 1. 핵심 개념

| 개념 | 설명 |
|------|------|
| **Seq** | 커밋마다 1씩 증가하는 번호. 헤더(0번 페이지)에 저장. `db.Seq()` |
| **DBID** | DB 생성 시 부여되는 16바이트 랜덤 ID. replica는 primary와 같은 DBID를 가짐 |
| **Frame** | 한 트랜잭션이 바꾼 페이지들의 전체 이미지(헤더 포함). replica에 그대로 적용하면 primary와 **바이트 단위로 동일한** 파일이 됨 |
| **복제 로그** | primary가 메모리에 유지하는 최근 Frame들 (기본 64MiB). 잠시 끊겼던 replica는 여기서 이어받음 |
| **스냅샷** | 특정 커밋 시점에 고정된 읽기 스냅샷의 전체 페이지 복사본. 쓰기를 멈추지 않음 |

### 온라인 스냅샷
스냅샷은 Pager의 읽기 스냅샷(특정 커밋 시점에 고정된 페이지 버전)을 복사합니다. 복사하는 동안에도 커밋은 계속되고 읽기도 막히지 않으며, 결과는 정확히 `BaseSeq` 시점의 상태입니다. replica는 같은 순간에 구독을 시작하므로 `BaseSeq` 이후의 커밋을 빠짐없이 이어받습니다. 단, 스냅샷을 복사하는 동안은 WAL 체크포인트가 그 시점 이후로 진행되지 않으므로 WAL이 커질 수 있습니다.

## 2. 온라인 백업

```go
err := db.Backup("/backup/app-2026-10-25.db")
```

- 결과물은 **그대로 열 수 있는 일반 DB 파일**입니다. 복구 = 그 파일을 열거나 원래 위치로 복사.
- 인메모리 DB(`:memory:`)도 파일로 백업할 수 있습니다.
- CLI: `bangdb> .backup /backup/app.db`

## 3. 미러링 (Primary / Replica)

### Primary

```go
import "github.com/knoxxr/bangdb/replication"

db, _ := bangdb.Open("app.db")
p := replication.NewPrimary(db, replication.PrimaryOptions{
	Token:   "shared-secret",   // 선택: replica 인증
	LogSize: 64 << 20,          // 재접속 replica를 위해 보관할 최근 커밋 크기
})
go p.ListenAndServe(":7070")    // TLS: p.Serve(tls.NewListener(ln, cfg))
// 애플리케이션은 평소처럼 db에 읽기/쓰기
```

### Replica

```go
rdb, _ := bangdb.Open("replica.db")          // 빈 파일이어도 됨 (":memory:"도 가능)
r := replication.StartReplica(rdb, "primary:7070", replication.ReplicaOptions{Token: "shared-secret"})

rows, _ := rdb.Query(`SELECT ...`)            // 읽기 분산
_, err := rdb.Exec(`INSERT ...`)              // → bangdb.ErrReadOnly

// read-your-writes: primary에 쓴 직후 같은 데이터를 replica에서 읽어야 할 때
seq := primaryDB.Seq()
r.WaitFor(ctx, seq)
```

동기화 절차 (replica가 연결할 때마다 자동):

```
replica ──hello(DBID, Seq, token)──▶ primary
                                     ├─ DBID 같고 Seq 이후 커밋이 로그에 있음 → 바로 스트리밍
                                     └─ 아니면(신규/너무 뒤처짐/다른 DB) →
                                          'S' 스냅샷 시작(BaseSeq) → 'P' 페이지들 → 'E' 끝
                                          → 임시 파일 완성 후 교체 설치
                                     이후 'F' 커밋 스트리밍, 유휴 시 'H' 하트비트(1초)
```

- 연결이 끊기면 자동 재접속(200ms → 최대 5초 백오프).
- replica 파일은 커밋마다 자체 WAL로 기록되므로 replica 프로세스가 죽어도 그대로 다시 열고 이어서 동기화합니다.
- 스냅샷은 `<replica>.snapshot` 임시 파일에 만든 뒤 완성되면 원자적으로 교체합니다. 그동안 replica는 기존 데이터로 계속 조회를 처리합니다(교체 순간에는 진행 중인 조회가 끝나기를 기다림).
- replica에서의 조회는 Frame 적용을 막지 않습니다(스냅샷 읽기). 무거운 분석 쿼리를 replica에서 실행해도 복제가 지연되지 않습니다.
- 모든 페이지·Frame은 CRC32로 검증합니다.

### 상태 확인

```go
r.Status()       // {Connected, Seq, PrimarySeq, Snapshots, LastError} → 지연 = PrimarySeq - Seq
p.Replicas()     // 연결된 replica 목록과 전송한 Seq
```

CLI:
```bash
bangdb -serve :7070 -token s3cret app.db              # primary + 셸
bangdb -replica-of host:7070 -token s3cret replica.db  # replica + 셸 (.repl 로 상태 확인)
```

### 장애 조치 (Failover)

```go
r.Promote()   // 복제 중단 + 쓰기 가능 + 새 DBID 부여
```

- 승격된 노드는 **새 DBID**를 받습니다. 다른 replica들이 이 노드를 따라오면 이력이 섞이지 않도록 스냅샷부터 다시 받습니다.
- 기존 primary가 살아나 계속 쓰지 않도록 막는 것(fencing)은 운영 측 책임입니다 (예: 기존 primary 프로세스 중지, 연결 주소 전환).

## 4. 보장과 한계

| 항목 | 내용 |
|------|------|
| 복제 방식 | **비동기**. primary 커밋 완료 후 replica에 전달됨. primary 장애 시 아직 전송되지 않은 마지막 커밋들은 replica에 없을 수 있음 |
| 일관성 | replica는 항상 primary의 어떤 커밋 시점과 정확히 같은 상태 (중간 상태 노출 없음) |
| 쓰기 확장 | 쓰기는 primary 한 곳. replica는 읽기 확장용 (샤딩 아님) |
| 전송 단위 | 페이지(4KB) 단위. 작은 UPDATE도 변경된 페이지 전체를 전송 |
| 로그 보관 | 메모리 기반. `LogSize`를 넘게 뒤처지면 다음 접속 시 스냅샷으로 재동기화 (자동). 스냅샷 전송 시간 동안 쌓이는 커밋이 로그 상한(최소 256MiB)을 넘으면 재시도됨 |
| 보안 | 토큰 인증 기본 제공. 암호화가 필요하면 `Serve(tls.Listener)` / `ReplicaOptions.Dial`로 TLS 연결 사용 |
| 계단식 복제 | replica를 승격하거나, primary가 아닌 노드에서 `NewPrimary`를 띄우면 가능 (승격 시 새 DBID) |
| VACUUM | primary에서 `VACUUM`을 실행하면 파일 배치가 바뀌므로 **새 DBID**가 부여되고 연결된 replica는 끊긴 뒤 자동으로 전체 스냅샷을 다시 받습니다 (`TestReplicaResyncsAfterVacuum`). replica에서는 실행할 수 없음(읽기 전용) |

## 5. 향후 확장 후보

- **동기/반동기 복제**: primary가 커밋 응답 전에 N개 replica의 수신 확인(ack)을 기다리는 모드 (`WaitFor`의 역방향).
- **연속 아카이빙(PITR)**: Frame 스트림을 파일로 저장하면 "백업 + 로그 재생"으로 특정 시점 복구 가능. 현재 프로토콜을 그대로 재사용할 수 있음.
- **원격 백업**: replica에 연결해 `.backup`을 실행하면 primary 부하 없이 백업 가능 (현재도 가능).
