# 안드로이드 앱에 내장하기

BangDB를 앱 안의 라이브러리(`.aar`)로 넣어 SQLite처럼 씁니다. 서버나 네트워크가 필요 없고, DB 파일은 앱 전용 저장소에 둡니다.

- 래퍼 패키지: [`mobile/`](../mobile) (별도 Go 모듈. 본체는 계속 외부 의존성 0)
- Java/Kotlin 패키지: `com.knoxxr.bangdb` — `Bangdb`(함수), `Database`, `Tx`, `Rows`, `Args`, `Result`, `Options`, `Migrations`
- 예제: [`mobile/sample/NoteStore.kt`](../mobile/sample/NoteStore.kt), [`NoteStoreJava.java`](../mobile/sample/NoteStoreJava.java)
- 최소 Android 5.0 (API 21), ABI: arm64-v8a, armeabi-v7a, x86, x86_64

## 1. `.aar` 받기

**CI 산출물(권장):** GitHub Actions의 각 실행 → Artifacts → `bangdb-android` → `bangdb-<버전>.aar`.

**직접 빌드:** Go, JDK 17, Android SDK + NDK가 있는 PC에서

```sh
cd mobile
go install golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind   # 버전은 mobile/go.mod에 고정
gomobile init
cd .. && scripts/build.sh aar        # → dist/bangdb-<버전>.aar
```

ABI를 줄여 크기를 낮추려면 `gomobile bind -target=android/arm64 ...` (최근 실기기는 대부분 arm64).

## 2. 앱에 추가

`app/libs/bangdb.aar`로 복사하고 `app/build.gradle.kts`에

```kotlin
dependencies {
    implementation(files("libs/bangdb.aar"))
}
```

## 3. 사용

```kotlin
val opts = Bangdb.newOptions()
opts.tempDir = context.cacheDir.path          // 큰 정렬·트랜잭션의 임시 파일
opts.cacheSize = 8L shl 20                    // 휴대폰은 8 MiB 정도로 충분
val db = Bangdb.openWithOptions(context.noBackupFilesDir.path + "/app.db", opts)

db.migrate(Bangdb.newMigrations()
    .add(1, "init", "CREATE TABLE note (id INTEGER PRIMARY KEY, body TEXT NOT NULL, price DECIMAL(10,2))")
    .add(2, "index", "CREATE INDEX note_body ON note (body)"))

val id = db.exec("INSERT INTO note (body, price) VALUES (?, ?)",
                 Bangdb.newArgs().addString("커피").addDecimal("4.50")).lastInsertID

val rows = db.query("SELECT id, body, price FROM note ORDER BY id", null)
try {
    while (rows.next()) {
        val body = rows.getString(1)
        val price = rows.getString(2)     // DECIMAL은 문자열로 받으면 정확
    }
} finally { rows.close() }

val json = db.queryJSON("SELECT count(*) AS n FROM note", null)   // {"columns":["n"],"rows":[[1]]}

val tx = db.begin()                        // 여러 변경을 한 번에(전부 아니면 전무)
try { tx.exec("...", null); tx.commit() } catch (e: Exception) { tx.rollback(); throw e }
```

| Go/SQL 값 | 읽기 | 쓰기 (`Args`) |
|---|---|---|
| INTEGER | `getLong` | `addLong` |
| REAL | `getDouble` | `addDouble` |
| TEXT | `getString` | `addString` |
| BLOB | `getBytes` | `addBytes` |
| DECIMAL | `getString`(정확) / `getDouble` | `addDecimal("12.50")` |
| BOOLEAN | `getBool` | `addBool` |
| NULL | `isNull` | `addNull` |

`columnType(i)`는 현재 행 값의 형(`Bangdb.TypeLong` 등), `columnIndex("name")`은 열 위치를 줍니다.

## 4. 주의할 점

- **한 프로세스에서 한 번만 열기.** 파일은 열려 있는 동안 잠깁니다(두 번째 열기는 예외, `Bangdb.isLockedError(e.message)`). `Application`이나 싱글턴에서 열어 두고 계속 씁니다. 여러 스레드에서 같은 `Database`를 써도 됩니다(쓰기는 자동으로 직렬화). `Tx`와 `Rows`는 한 스레드에서만 씁니다.
- **UI 스레드에서 호출하지 않기.** `Dispatchers.IO` 등 백그라운드에서 부릅니다.
- **저장 위치는 앱 전용 저장소**(`noBackupFilesDir` 또는 `filesDir`). 공용 저장소(`/sdcard`)는 파일 잠금·fsync가 보장되지 않습니다. `-wal` 파일이 같은 폴더에 생깁니다.
- **자동 백업**: `filesDir`에 두면 Android 자동 백업이 열린 DB를 복사해 깨진 사본이 될 수 있습니다. `noBackupFilesDir`에 두고 백업이 필요하면 `db.backup(path)` + `db.verifyBackup(path)`로 만든 사본을 올립니다.
- **암호화**: `opts.encryptionKey`에 32바이트 키. 키는 Android Keystore로 감싸 보관합니다(DB 안에 두지 않음). 키 없이 열면 예외.
- **오류**: 모든 실패는 Java 예외(`Exception`)로 옵니다. 메시지로 분류: `Bangdb.isConstraintError`, `isBusyError`, `isLockedError`.
- **종료**: 앱이 DB를 더 쓰지 않을 때 `db.close()`. 프로세스가 갑자기 죽어도 커밋된 데이터는 다음 열기 때 WAL에서 복구됩니다.
- **크기**: `.aar`는 ABI 4개 포함 약 20 MB 안팎, 앱 번들(AAB)로 배포하면 기기별로 한 ABI(약 5 MB)만 내려갑니다.

## 5. 검증 상태

- 래퍼 API: `mobile/bangdb_test.go` (암호화·마이그레이션·트랜잭션·형 변환·JSON·잠금·백업)
- 바인딩: CI `android` 작업이 `gomobile bind`로 `.aar`를 만들고, 예제(Java·Kotlin)를 그 `.aar`와 `android.jar`에 대해 컴파일
- 실기기·에뮬레이터 실행 시험은 아직 CI에 없음
