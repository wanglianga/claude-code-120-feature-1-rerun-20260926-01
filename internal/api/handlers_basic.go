package api

import "net/http"

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m := map[string]any{}
	q := func(k, sql string) {
		var v int
		_ = s.pool.QueryRow(ctx, sql).Scan(&v)
		m[k] = v
	}
	q("raw_total", `SELECT count(*) FROM raw_material_batches`)
	q("raw_locked", `SELECT count(*) FROM raw_material_batches WHERE status='locked'`)
	q("finished_total", `SELECT count(*) FROM finished_batches`)
	q("events_open", `SELECT count(*) FROM quality_events WHERE status='open'`)
	q("recalls_active", `SELECT count(*) FROM recalls WHERE status='executing'`)
	q("orders_recalled", `SELECT count(*) FROM orders WHERE status='recalled'`)
	q("returns_pending", `SELECT count(*) FROM return_goods WHERE disposition IS NULL`)
	q("comp_pending", `SELECT count(*) FROM customer_compensations WHERE status='pending'`)
	q("deduction_pending", `SELECT count(*) FROM supplier_deductions WHERE status<>'deducted'`)
	var stock, quarantine float64
	_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(qty_kg),0) FROM inventory WHERE status='available'`).Scan(&stock)
	_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(qty_kg),0) FROM inventory WHERE status='quarantined'`).Scan(&quarantine)
	m["stock_kg"] = stock
	m["quarantine_kg"] = quarantine
	writeJSON(w, 200, m)
}

func (s *Server) listSuppliers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(),
		`SELECT id,code,name,contact,status FROM suppliers ORDER BY code`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var code, name, contact, status string
		if err := rows.Scan(&id, &code, &name, &contact, &status); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{"id": id, "code": code, "name": name, "contact": contact, "status": status})
	}
	writeJSON(w, 200, out)
}

func (s *Server) listLines(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `SELECT id,code,name,active FROM production_lines ORDER BY code`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var code, name string
		var active bool
		_ = rows.Scan(&id, &code, &name, &active)
		out = append(out, map[string]any{"id": id, "code": code, "name": name, "active": active})
	}
	writeJSON(w, 200, out)
}

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `SELECT id,code,name,ctype FROM channels ORDER BY code`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var code, name, ctype string
		_ = rows.Scan(&id, &code, &name, &ctype)
		out = append(out, map[string]any{"id": id, "code": code, "name": name, "ctype": ctype})
	}
	writeJSON(w, 200, out)
}

func (s *Server) listSeasonings(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(),
		`SELECT id,batch_no,name,supplier,expiry_date::text FROM seasoning_batches ORDER BY batch_no`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var bn, name, sup, exp string
		_ = rows.Scan(&id, &bn, &name, &sup, &exp)
		out = append(out, map[string]any{"id": id, "batch_no": bn, "name": name, "supplier": sup, "expiry_date": exp})
	}
	writeJSON(w, 200, out)
}
