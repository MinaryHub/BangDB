// The same API from Java.
package com.example.notes;

import com.knoxxr.bangdb.Bangdb;
import com.knoxxr.bangdb.Database;
import com.knoxxr.bangdb.Rows;

public final class NoteStoreJava {
    public static long count(String path) throws Exception {
        Database db = Bangdb.open(path);
        try {
            db.exec("CREATE TABLE IF NOT EXISTS note (id INTEGER PRIMARY KEY, body TEXT)", null);
            db.exec("INSERT INTO note (body) VALUES (?)", Bangdb.newArgs().addString("hi"));
            Rows rows = db.query("SELECT count(*) FROM note", null);
            long n = rows.next() ? rows.getLong(0) : 0;
            rows.close();
            return n;
        } finally {
            db.close();
        }
    }
}
