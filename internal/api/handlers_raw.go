package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) listRawBatches(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT rm.id, rm.batch_no, sp.code, sp.name, rm.species,
		       rm.catch_or_production_date::text, rm.received_at::text,
		       rm.inbound_temp_c, rm.quarantine_cert, rm.box_no, rm.location,
		       rm.qty_kg, rm.status
		FROM raw_material_batches rm
		LEFT JOIN suppliers sp ON sp.id=rm.supplier_id
		ORDER BY rm.received_at DESC`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var bn, species, catchDate, receivedAt, cert, boxNo, location, status string
		var inboundTemp, qty float64
		var scode, sname *string
		if err := rows.Scan(&id, &bn, &scode, &sname, &species,
			&catchDate, &receivedAt, &inboundTemp, &cert, &boxNo, &location,
			&qty, &status); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		m := map[string]any{
			"id": id, "batch_no": bn, "species": species, "catch_date": catchDate,
			"received_at": receivedAt, "inbound_temp_c": inboundTemp,
			"quarantine_cert": cert, "box_no": boxNo, "location": location,
			"qty_kg": qty, "status": status,
		}
		if scode != nil {
			m["supplier_code"] = *scode
			m["supplier_name"] = *sname
		}
		out = append(out, m)
	}
	writeJSON(w, 200, out)
}

type rawBatchReq struct {
	BatchNo        string  `json:"batch_no"`
	SupplierCode   string  `json:"supplier_code"`
	Species        string  `json:"species"`
	CatchDate      string  `json:"catch_date"`
	InboundTemp    float64 `json:"inbound_temp_c"`
	QuarantineCert string  `json:"quarantine_cert"`
	BoxNo          string  `json:"box_no"`
	Location       string  `json:"location"`
	QtyKg          float64 `json:"qty_kg"`
}

func (s *Server) createRawBatch(w http.ResponseWriter, r *http.Request) {
	var req rawBatchReq
	if err := decode(r, &req); err != nil || req.BatchNo == "" {
		writeErr(w, 400, "请求字段错误或缺少 batch_no")
		return
	}
	u := currentUser(r)
	// 冻品入库温度限值 -18℃：超标自动锁定
	status := "in_storage"
	alarm := false
	if req.InboundTemp > -18 {
		status = "locked"
		alarm = true
	}
	var id int64
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO raw_material_batches
		  (batch_no,supplier_id,species,catch_or_production_date,inbound_temp_c,
		   quarantine_cert,box_no,location,qty_kg,status,created_by)
		SELECT $1,id,$2,$3,$4,$5,$6,$7,$8,$9,$10 FROM suppliers WHERE code=$11
		RETURNING id`,
		req.BatchNo, req.Species, req.CatchDate, req.InboundTemp,
		req.QuarantineCert, req.BoxNo, req.Location, req.QtyKg, status, u.ID,
		req.SupplierCode).Scan(&id)
	if err != nil {
		writeErr(w, 400, "入库失败："+err.Error())
		return
	}
	if alarm {
		_, _ = s.pool.Exec(r.Context(), `
			INSERT INTO temp_logs(raw_batch_id,temp_c,operator,note,alarm)
			VALUES($1,$2,$3,$4,true)`,
			id, req.InboundTemp, u.Username, "入库测温高于 -18℃ 限值，自动锁定待判")
	}
	writeJSON(w, 201, map[string]any{"id": id, "batch_no": req.BatchNo, "status": status,
		"temp_alarm": alarm})
}

type tempLogReq struct {
	TempC    float64 `json:"temp_c"`
	Operator string  `json:"operator"`
	Note     string  `json:"note"`
}

func (s *Server) addTempLog(w http.ResponseWriter, r *http.Request) {
	bn := chi.URLParam(r, "batchNo")
	var req tempLogReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "字段错误")
		return
	}
	alarm := req.TempC > -18
	var id int64
	if err := s.pool.QueryRow(r.Context(),
		`INSERT INTO temp_logs(raw_batch_id,temp_c,operator,note,alarm)
		 SELECT id,$2,$3,$4,$5 FROM raw_material_batches WHERE batch_no=$1 RETURNING id`,
		bn, req.TempC, req.Operator, req.Note, alarm).Scan(&id); err != nil {
		writeErr(w, 404, "原料批次不存在")
		return
	}
	if alarm {
		_, _ = s.pool.Exec(r.Context(),
			`UPDATE raw_material_batches SET status='locked' WHERE batch_no=$1 AND status='in_storage'`, bn)
	}
	writeJSON(w, 201, map[string]any{"id": id, "alarm": alarm})
}

// rawTrace 正向追溯：原料批次 → 成品 → 渠道/订单 → 留样 → 事件/召回
func (s *Server) rawTrace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	batchNo := chi.URLParam(r, "batchNo")

	var supplier *string
	var bn, species, catchDate, cert, boxNo, location, status string
	var inboundTemp, qty float64
	row := s.pool.QueryRow(ctx, `
		SELECT rm.batch_no, sp.name, rm.species, rm.catch_or_production_date::text,
		       rm.inbound_temp_c, rm.quarantine_cert, rm.box_no, rm.location,
		       rm.qty_kg, rm.status
		FROM raw_material_batches rm LEFT JOIN suppliers sp ON sp.id=rm.supplier_id
		WHERE rm.batch_no=$1`, batchNo)
	if err := row.Scan(&bn, &supplier, &species, &catchDate,
		&inboundTemp, &cert, &boxNo, &location, &qty, &status); err != nil {
		writeErr(w, 404, "原料批次不存在")
		return
	}
	m := map[string]any{
		"batch_no": bn, "species": species, "catch_date": catchDate,
		"inbound_temp_c": inboundTemp, "quarantine_cert": cert,
		"box_no": boxNo, "location": location, "qty_kg": qty, "status": status,
	}
	if supplier != nil {
		m["supplier_name"] = *supplier
	}
	raw := m

	queryMaps := func(sql string, args ...any) []map[string]any {
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return []map[string]any{}
		}
		defer rows.Close()
		return scanMaps(rows)
	}

	tempLogs := queryMaps(`
		SELECT temp_c, recorded_at::text, operator, note, alarm
		FROM temp_logs WHERE raw_batch_id=(SELECT id FROM raw_material_batches WHERE batch_no=$1)
		ORDER BY recorded_at`, batchNo)

	finished := queryMaps(`
		SELECT f.batch_no, f.product_name, f.packaging_spec, f.qty_kg, f.status,
		       COALESCE(f.label_mark,'') AS label_mark,
		       COALESCE(f.label_text,'') AS label_text,
		       COALESCE(f.channel_restriction,'') AS channel_restriction,
		       l.qty_kg AS used_qty_kg
		FROM batch_links l JOIN finished_batches f ON f.id=l.finished_batch_id
		WHERE l.raw_batch_id=(SELECT id FROM raw_material_batches WHERE batch_no=$1)
		ORDER BY f.produced_at`, batchNo)

	thawLocks := queryMaps(`
		SELECT l.lock_no,l.duration_minutes,l.status,l.locked_at::text,
		       l.retest_center_temp_c,l.retest_sensory,l.retest_result,l.retest_at::text,
		       l.label_text,l.channel_restriction,l.allow_rework,
		       f.batch_no AS semi_finished_batch_no,l.quarantine_location,
		       l.quarantine_result,l.quarantined_at::text
		FROM thaw_locks l
		LEFT JOIN finished_batches f ON f.id=l.semi_finished_id
		WHERE l.raw_batch_id=(SELECT id FROM raw_material_batches WHERE batch_no=$1)
		ORDER BY l.locked_at DESC`, batchNo)

	samples := queryMaps(`
		SELECT f.batch_no AS finished_batch_no, sm.sample_no, sm.result,
		       sm.tested_at::text, sm.lab_note
		FROM samples sm JOIN finished_batches f ON f.id=sm.finished_batch_id
		WHERE f.id IN (SELECT finished_batch_id FROM batch_links
		               WHERE raw_batch_id=(SELECT id FROM raw_material_batches WHERE batch_no=$1))
		ORDER BY sm.taken_at`, batchNo)

	shipments := queryMaps(`
		SELECT sh.ship_no, ch.name AS channel, ch.ctype,
		       f.batch_no AS finished_batch_no, si.boxes, si.qty_kg
		FROM shipment_items si
		JOIN shipments sh ON sh.id=si.shipment_id
		JOIN channels ch ON ch.id=sh.channel_id
		JOIN finished_batches f ON f.id=si.finished_batch_id
		WHERE f.id IN (SELECT finished_batch_id FROM batch_links
		               WHERE raw_batch_id=(SELECT id FROM raw_material_batches WHERE batch_no=$1))
		ORDER BY sh.shipped_at DESC`, batchNo)

	orders := queryMaps(`
		SELECT o.order_no, ch.name AS channel, f.batch_no AS finished_batch_no,
		       o.box_no, o.qty, o.customer, o.status, o.sold_at::text
		FROM orders o
		LEFT JOIN channels ch ON ch.id=o.channel_id
		JOIN finished_batches f ON f.id=o.finished_batch_id
		WHERE f.id IN (SELECT finished_batch_id FROM batch_links
		               WHERE raw_batch_id=(SELECT id FROM raw_material_batches WHERE batch_no=$1))
		ORDER BY o.sold_at DESC`, batchNo)

	events := queryMaps(`
		SELECT event_no, etype, severity, description, status, created_at::text
		FROM quality_events
		WHERE raw_batch_id=(SELECT id FROM raw_material_batches WHERE batch_no=$1)
		   OR finished_batch_id IN (SELECT finished_batch_id FROM batch_links
		                            WHERE raw_batch_id=(SELECT id FROM raw_material_batches WHERE batch_no=$1))
		ORDER BY created_at DESC`, batchNo)

	recalls := queryMaps(`
		SELECT DISTINCT rc.recall_no, rc.level, rc.reason, rc.status, rc.initiated_at::text
		FROM recalls rc
		JOIN quality_events e ON e.id=rc.event_id
		WHERE e.raw_batch_id=(SELECT id FROM raw_material_batches WHERE batch_no=$1)
		   OR e.finished_batch_id IN (SELECT finished_batch_id FROM batch_links
		                              WHERE raw_batch_id=(SELECT id FROM raw_material_batches WHERE batch_no=$1))
		ORDER BY rc.initiated_at::text DESC`, batchNo)

	writeJSON(w, 200, map[string]any{
		"raw": raw, "temp_logs": tempLogs, "finished": finished,
		"thaw_locks": thawLocks,
		"samples":    samples, "shipments": shipments, "orders": orders,
		"events": events, "recalls": recalls,
	})
}
