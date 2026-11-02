// Example: a small note store on BangDB, for an Android app.
// Add the library to app/libs and, in app/build.gradle.kts:
//     dependencies { implementation(files("libs/bangdb.aar")) }
package com.example.notes

import android.content.Context
import com.knoxxr.bangdb.Bangdb
import com.knoxxr.bangdb.Database

data class Note(val id: Long, val body: String, val price: String, val done: Boolean)

class NoteStore private constructor(private val db: Database) {

    companion object {
        @Volatile private var instance: NoteStore? = null

        // Open once per process (the file is locked while open) and keep it.
        fun get(context: Context): NoteStore = instance ?: synchronized(this) {
            instance ?: open(context.applicationContext).also { instance = it }
        }

        private fun open(context: Context): NoteStore {
            val opts = Bangdb.newOptions()
            opts.tempDir = context.cacheDir.path // large sorts and transactions spill here
            opts.cacheSize = 8L shl 20
            // opts.encryptionKey = keyFromAndroidKeystore()  // 32 bytes, optional
            val db = Bangdb.openWithOptions(context.noBackupFilesDir.path + "/notes.db", opts)
            db.migrate(
                Bangdb.newMigrations()
                    .add(1, "notes", """
                        CREATE TABLE note (
                          id    INTEGER PRIMARY KEY,
                          body  TEXT NOT NULL,
                          price DECIMAL(10,2) DEFAULT 0,
                          done  BOOLEAN DEFAULT 0
                        )""")
                    .add(2, "by state", "CREATE INDEX note_done ON note (done)")
            )
            return NoteStore(db)
        }
    }

    // Call from a background thread (e.g. Dispatchers.IO), not the UI thread.
    fun add(body: String, price: String): Long =
        db.exec(
            "INSERT INTO note (body, price) VALUES (?, ?)",
            Bangdb.newArgs().addString(body).addDecimal(price)
        ).lastInsertID

    fun setDone(id: Long, done: Boolean) {
        db.exec("UPDATE note SET done = ? WHERE id = ?", Bangdb.newArgs().addBool(done).addLong(id))
    }

    fun open(): List<Note> {
        val rows = db.query("SELECT id, body, price, done FROM note WHERE done = 0 ORDER BY id", null)
        val out = ArrayList<Note>()
        try {
            while (rows.next()) {
                out.add(Note(rows.getLong(0), rows.getString(1), rows.getString(2), rows.getBool(3)))
            }
        } finally {
            rows.close()
        }
        return out
    }

    // Several changes, all or nothing.
    fun importAll(bodies: List<String>) {
        val tx = db.begin()
        try {
            for (b in bodies) tx.exec("INSERT INTO note (body) VALUES (?)", Bangdb.newArgs().addString(b))
            tx.commit()
        } catch (e: Exception) {
            tx.rollback()
            throw e
        }
    }

    fun total(): String = db.queryJSON("SELECT sum(price) AS total FROM note", null) // {"columns":["total"],"rows":[[12.50]]}

    fun backup(context: Context): String {
        val path = context.filesDir.path + "/notes-backup.db"
        db.backup(path)
        db.verifyBackup(path)
        return path
    }

    fun isDuplicate(e: Exception) = Bangdb.isConstraintError(e.message ?: "")
}
