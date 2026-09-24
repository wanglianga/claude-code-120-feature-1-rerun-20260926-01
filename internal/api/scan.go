package api

import (
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// scanMaps 把任意查询结果扫描为 []map（列名取自结果集元数据）。
func scanMaps(rows pgx.Rows) []map[string]any {
	out := []map[string]any{}
	fd := rows.FieldDescriptions()
	names := make([]string, len(fd))
	for i, f := range fd {
		names[i] = f.Name
	}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			continue
		}
		m := map[string]any{}
		for i, v := range vals {
			m[names[i]] = normalize(v)
		}
		out = append(out, m)
	}
	return out
}

func normalize(v any) any {
	switch t := v.(type) {
	case time.Time:
		return t.Format("2006-01-02 15:04:05Z07:00")
	case []byte:
		return string(t)
	case fmt.Stringer:
		return t.String()
	default:
		return v
	}
}
