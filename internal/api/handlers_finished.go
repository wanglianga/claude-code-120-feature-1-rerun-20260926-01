package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (s *Server) listFinished(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT f.id,f.batch_no,f.product_name,f.packaging_spec,f.qty_kg,
		       f.cold_storage_location,f.status,f.produced_at::text,
		       (SELECT COALESCE(sum(qty_kg),0) FROM inventory i
		         WHERE i.finished_batch_id=f.id AND i.status='available') AS stock_kg,
		       (SELECT result FROM samples sm WHERE sm.finished_batch_id=f.id
		         ORDER BY sm.taken_at DESC LIMIT 1) AS latest_sample
		FROM finished_batches f ORDER BY f.produced_at DESC`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

type sampleReq struct {
	SampleNo  string `json:"sample_no"`
	Result    string `json:"result"`
	LabNote   string `json:"lab_note"`
	Inspector string `json:"inspector"`
}

func (s *Server) addSample(w http.ResponseWriter, r *http.Request) {
	bn := chi.URLParam(r, "batchNo")
	var req sampleReq
	if err := decode(r, &req); err != nil ||
		(req.Result != "qualified" && req.Result != "unqualified") {
		writeErr(w, 400, "result 必须为 qualified/unqualified")
		return
	}
	ctx := r.Context()
	if req.SampleNo != "" {
		tag, err := s.pool.Exec(ctx, `
			UPDATE samples SET result=$2,tested_at=now(),lab_note=$3,inspector=$4
			WHERE sample_no=$1
			  AND finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$5)`,
			req.SampleNo, req.Result, req.LabNote, req.Inspector, bn)
		if err != nil || tag.RowsAffected() == 0 {
			writeErr(w, 404, "留样记录不存在或不属于该成品批次")
			return
		}
		writeJSON(w, 200, map[string]any{"sample_no": req.SampleNo, "result": req.Result})
		return
	}
	no := docNo("SP")
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO samples(sample_no,finished_batch_id,result,tested_at,lab_note,inspector)
		SELECT $1,id,$2,now(),$3,$4 FROM finished_batches WHERE batch_no=$5`,
		no, req.Result, req.LabNote, req.Inspector, bn); err != nil {
		writeErr(w, 400, "留样登记失败："+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"sample_no": no, "result": req.Result})
}

// finishedTrace 反向追溯：成品 → 原料批次（供应商/入库/解冻）→ 留样 → 渠道/订单 → 事件/召回 → 库存/退货
func (s *Server) finishedTrace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bn := chi.URLParam(r, "batchNo")

	finRows, err := s.pool.Query(ctx, `
		SELECT f.batch_no,f.product_name,f.packaging_spec,f.qty_kg,
		       f.cold_storage_location,f.status,f.produced_at::text
		FROM finished_batches f WHERE f.batch_no=$1`, bn)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	fin := scanMaps(finRows)
	finRows.Close()
	if len(fin) == 0 {
		writeErr(w, 404, "成品批次不存在")
		return
	}

	qm := func(sql string, args ...any) []map[string]any {
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return []map[string]any{}
		}
		defer rows.Close()
		return scanMaps(rows)
	}

	raws := qm(`
		SELECT rm.batch_no, sp.code AS supplier_code, sp.name AS supplier_name,
		       rm.species, rm.catch_or_production_date::text, rm.inbound_temp_c,
		       rm.quarantine_cert, rm.box_no, rm.location, rm.status, l.qty_kg AS used_qty_kg
		FROM batch_links l
		JOIN raw_material_batches rm ON rm.id=l.raw_batch_id
		LEFT JOIN suppliers sp ON sp.id=rm.supplier_id
		WHERE l.finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$1)`, bn)

	thaws := qm(`
		SELECT rm.batch_no AS raw_batch_no, t.start_at::text, t.end_at::text,
		       t.duration_minutes, t.center_temp_c, t.water_batch_no, t.operator
		FROM thaw_records t
		JOIN production_plans p ON p.id=t.plan_id
		JOIN raw_material_batches rm ON rm.id=t.raw_batch_id
		WHERE p.id=(SELECT plan_id FROM finished_batches WHERE batch_no=$1)`, bn)

	samples := qm(`
		SELECT sample_no,result,taken_at::text,tested_at::text,lab_note,inspector
		FROM samples WHERE finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$1)
		ORDER BY taken_at`, bn)

	shipments := qm(`
		SELECT sh.ship_no,ch.code AS channel_code,ch.name AS channel,ch.ctype,
		       sh.shipped_at::text,si.boxes,si.qty_kg,sh.status
		FROM shipment_items si
		JOIN shipments sh ON sh.id=si.shipment_id
		JOIN channels ch ON ch.id=sh.channel_id
		WHERE si.finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$1)
		ORDER BY sh.shipped_at DESC`, bn)

	orders := qm(`
		SELECT o.order_no,ch.name AS channel,o.box_no,o.qty,o.customer,o.status,o.sold_at::text
		FROM orders o LEFT JOIN channels ch ON ch.id=o.channel_id
		WHERE o.finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$1)
		ORDER BY o.sold_at DESC`, bn)

	inventory := qm(`
		SELECT location,qty_kg,status,updated_at::text FROM inventory
		WHERE finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$1)`, bn)

	returns := qm(`
		SELECT return_no,box_no,qty_kg,arrived_at::text,disposition,decided_by,decided_at::text,note
		FROM return_goods
		WHERE finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$1)
		ORDER BY arrived_at DESC`, bn)

	events := qm(`
		SELECT event_no,etype,severity,description,status,created_at::text
		FROM quality_events
		WHERE finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$1)
		   OR raw_batch_id IN (SELECT raw_batch_id FROM batch_links
		                       WHERE finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$1))
		ORDER BY created_at DESC`, bn)

	recalls := qm(`
		SELECT rc.recall_no,rc.level,rc.reason,rc.status,rc.initiated_at::text
		FROM recalls rc JOIN quality_events e ON e.id=rc.event_id
		WHERE e.finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$1)
		   OR e.raw_batch_id IN (SELECT raw_batch_id FROM batch_links
		                         WHERE finished_batch_id=(SELECT id FROM finished_batches WHERE batch_no=$1))
		ORDER BY rc.initiated_at DESC`, bn)

	writeJSON(w, 200, map[string]any{
		"finished": fin[0], "raw_materials": raws, "thaw_records": thaws,
		"samples": samples, "shipments": shipments, "orders": orders,
		"inventory": inventory, "returns": returns,
		"events": events, "recalls": recalls,
	})
}
