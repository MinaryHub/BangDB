package bangdb

import (
	"strings"
	"testing"
)

func TestJSONFunctions(t *testing.T) {
	db := open(t)
	for q, want := range map[string]string{
		`SELECT json(' { "a" : [1, 2.5, "x"], "b": null } ')`:                       `{"a":[1,2.5,"x"],"b":null}`,
		`SELECT json_valid('{"a":1}'), json_valid('{a:1}'), json_valid(NULL)`:       "1|0|NULL",
		`SELECT json_extract('{"a":{"b":[10,20,30]}}', '$.a.b[1]')`:                 "20",
		`SELECT json_extract('{"a":{"b":[10,20,30]}}', '$.a.b[#-1]')`:               "30",
		`SELECT json_extract('{"a":{"b":[10,20,30]}}', '$.a')`:                      `{"b":[10,20,30]}`,
		`SELECT json_extract('{"a":"txt","t":true,"f":1.5}', '$.a', '$.t', '$.zz')`: `["txt",true,null]`,
		`SELECT json_extract('{"a":true}', '$.a'), json_extract('{"a":1}', '$.b')`:  "1|NULL",
		`SELECT json_extract('{"a b":1}', '$."a b"')`:                               "1",
		`SELECT typeof(json_extract('[1, 1.0, "1"]', '$[0]')), typeof(json_extract('[1, 1.0, "1"]', '$[1]')),
			typeof(json_extract('[1, 1.0, "1"]', '$[2]'))`: "integer|real|text",
		`SELECT json_type('{"a":[1,2.5,"s",null,true,{}]}', '$.a'), json_type('[1,2.5,"s",null,true,{}]', '$[0]'),
			json_type('[1,2.5,"s",null,true,{}]', '$[1]'), json_type('[1,2.5,"s",null,true,{}]', '$[3]'),
			json_type('[1,2.5,"s",null,true,{}]', '$[4]'), json_type('[1,2.5,"s",null,true,{}]', '$[5]'),
			json_type('[]', '$[0]')`: "array|integer|real|null|true|object|NULL",
		`SELECT json_array_length('[1,2,3]'), json_array_length('{"a":[1]}', '$.a'), json_array_length('{}')`:   "3|1|0",
		`SELECT json_array(1, 2.0, 'x', NULL, json_array(3), json('{"k":1}'), '[4]')`:                           `[1,2.0,"x",null,[3],{"k":1},"[4]"]`,
		`SELECT json_object('a', 1, 'b', json_object('c', 'd"q'), 'a', 2)`:                                      `{"a":2,"b":{"c":"d\"q"}}`,
		`SELECT json_set('{"a":1}', '$.a', 2, '$.b.c', 3, '$.arr[#]', 4)`:                                       `{"a":2,"b":{"c":3},"arr":[4]}`,
		`SELECT json_insert('{"a":1,"l":[1]}', '$.a', 2, '$.z', 'new', '$.l[#]', 9)`:                            `{"a":1,"l":[1,9],"z":"new"}`,
		`SELECT json_replace('{"a":1}', '$.a', json_array(1), '$.z', 2)`:                                        `{"a":[1]}`,
		`SELECT json_remove('{"a":1,"b":[1,2,3]}', '$.a', '$.b[1]'), json_remove('[1]', '$')`:                   `{"b":[1,3]}|NULL`,
		`SELECT json_patch('{"a":1,"b":{"c":2,"d":3}}', '{"a":null,"b":{"d":4,"e":5},"f":6}')`:                  `{"b":{"c":2,"d":4,"e":5},"f":6}`,
		`SELECT json_quote('a"b'), json_quote(3.5), json_quote(NULL)`:                                           `"a\"b"|3.5|null`,
		`SELECT '{"a":{"b":"x"}}' -> '$.a', '{"a":{"b":"x"}}' -> 'a' ->> 'b', '[5,6,7]' ->> -1, '[5,6,7]' -> 0`: `{"b":"x"}|x|7|5`,
		`SELECT '{"a":"x"}' -> 'a', '{"a":"x"}' ->> 'a', '{"a":1}' -> 'zz'`:                                     `"x"|x|NULL`,
		`SELECT json_object('v', '{"a":[1]}' -> 'a', 's', '{"a":[1]}' ->> 'a')`:                                 `{"v":[1],"s":"[1]"}`,
		`SELECT '{"n":2}' ->> 'n' + 1`: "3",
	} {
		expect(t, db, q, want)
	}

	exec(t, db, "CREATE TABLE doc (id INTEGER PRIMARY KEY, body TEXT, grp TEXT)")
	exec(t, db, `INSERT INTO doc VALUES (1, '{"name":"ann","tags":["x","y"],"age":31}', 'a'),
		(2, '{"name":"bob","tags":[],"age":25}', 'a'), (3, '{"name":"cy"}', 'b')`)
	expect(t, db, "SELECT id FROM doc WHERE body ->> 'name' = 'bob'", "2")
	expect(t, db, "SELECT id, json_array_length(body, '$.tags') FROM doc WHERE body ->> 'age' > 26 ORDER BY id", "1|2")
	expect(t, db, "SELECT body ->> 'name' FROM doc ORDER BY body ->> 'age' DESC", "ann;bob;cy")
	expect(t, db, "UPDATE doc SET body = json_set(body, '$.age', 26) WHERE id = 3 RETURNING body",
		`{"name":"cy","age":26}`)

	// Aggregates.
	expect(t, db, "SELECT grp, json_group_array(body ->> 'name') FROM doc GROUP BY grp ORDER BY grp",
		`a|["ann","bob"];b|["cy"]`)
	expect(t, db, "SELECT json_group_object(body ->> 'name', body -> 'age') FROM doc",
		`{"ann":31,"bob":25,"cy":26}`)
	expect(t, db, "SELECT json_group_array(json_object('id', id)) FROM doc WHERE id < 3",
		`[{"id":1},{"id":2}]`)
	expect(t, db, "SELECT json_group_array(body -> 'tags') FROM doc", `[["x","y"],[],null]`)
	expect(t, db, "SELECT json_group_array(id), json_group_object('k', id) FROM doc WHERE id > 9", "[]|{}")

	for q, want := range map[string]string{
		`SELECT json('{bad')`:                   "malformed JSON",
		`SELECT json_extract('{}', 'a')`:        "bad JSON path",
		`SELECT json_extract('{}', '$.a[x]')`:   "bad JSON path",
		`SELECT json_object('a')`:               "even number",
		`SELECT json_object(1, 2)`:              "TEXT",
		`SELECT json_array(x'00')`:              "BLOB",
		`SELECT json_group_object(id) FROM doc`: "two arguments",
	} {
		rows, err := db.Query(q)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", q, err, want)
		}
	}
}
