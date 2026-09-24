package api

import (
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// ---------- 品控事件 ----------

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT e.id,e.event_no,e.etype,e.severity,
		       rm.batch_no AS raw_batch_no,f.batch_no AS finished_batch_no,
		       o.order_no,e.description,e.status,e.created_at::text
		FROM quality_events e
		LEFT JOIN raw_material_batches rm ON rm.id=e.raw_batch_id
		LEFT JOIN finished_batches f ON f.id=e.finished_batch_id
		LEFT JOIN orders o ON o.id=e.order_id
		ORDER BY e.created_at DESC`)
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

type eventReq struct {
	EType       string `json:"etype"`
	Severity    string `json:"severity"`
	RawBatch    string `json:"raw_batch_no"`
	Finished    string `json:"finished_batch_no"`
	OrderNo     string `json:"order_no"`
	Description string `json:"description"`
}

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
	var req eventReq
	if err := decode(r, &req); err != nil || req.EType == "" {
		writeErr(w, 400, "缺少 etype")
		return
	}
	if req.Severity == "" {
		req.Severity = "major"
	}
	u := currentUser(r)
	evNo := docNo("EV")
	var id int64
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO quality_events(event_no,etype,severity,raw_batch_id,finished_batch_id,order_id,description,created_by)
		SELECT $1,$2,$3,
		  (SELECT id FROM raw_material_batches WHERE batch_no=$4),
		  (SELECT id FROM finished_batches WHERE batch_no=$5),
		  (SELECT id FROM orders WHERE order_no=$6),
		  $7,$8
		RETURNING id`,
		evNo, req.EType, req.Severity, req.RawBatch, req.Finished, req.OrderNo,
		req.Description, u.ID).Scan(&id)
	if err != nil {
		writeErr(w, 400, "事件登记失败："+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "event_no": evNo})
}

// ---------- 召回 ----------

type recallReq struct {
	EventNo string `json:"event_no"`
	Level   string `json:"level"`
	Reason  string `json:"reason"`
}

func (s *Server) createRecall(w http.ResponseWriter, r *http.Request) {
	var req recallReq
	if err := decode(r, &req); err != nil || req.EventNo == "" {
		writeErr(w, 400, "缺少 event_no")
		return
	}
	if req.Level == "" {
		req.Level = "class2"
	}
	u := currentUser(r)
	ctx := r.Context()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	// 1) 定位事件
	var eventID int64
	var evRaw, evFin, evOrder *int64
	var etype, desc string
	if err := tx.QueryRow(ctx, `
		SELECT id,etype,COALESCE(description,''),raw_batch_id,finished_batch_id,order_id
		FROM quality_events WHERE event_no=$1 AND status='open'`,
		req.EventNo).Scan(&eventID, &etype, &desc, &evRaw, &evFin, &evOrder); err != nil {
		writeErr(w, 404, "未找到可召回的开放事件："+req.EventNo)
		return
	}

	// 2) 圈定原料/成品范围（原料↔成品双向闭包）：
	//    原料事件向下展开到关联成品；成品事件向上反查原料，再展开到共线成品。
	var rawIDs []int64
	if evRaw != nil {
		rawIDs = append(rawIDs, *evRaw)
	}
	var finIDs []int64
	if evFin != nil {
		finIDs = append(finIDs, *evFin)
	}
	// 成品事件：先反查该成品使用的原料
	if evFin != nil {
		r0, _ := tx.Query(ctx,
			`SELECT DISTINCT raw_batch_id FROM batch_links WHERE finished_batch_id=$1`, *evFin)
		for r0.Next() {
			var id int64
			_ = r0.Scan(&id)
			rawIDs = appendUnique(rawIDs, id)
		}
		r0.Close()
	}
	// 原料 → 关联成品
	if len(rawIDs) > 0 {
		rf, _ := tx.Query(ctx,
			`SELECT DISTINCT finished_batch_id FROM batch_links WHERE raw_batch_id=ANY($1::bigint[])`, rawIDs)
		for rf.Next() {
			var id int64
			_ = rf.Scan(&id)
			finIDs = appendUnique(finIDs, id)
		}
		rf.Close()
	}
	// 成品 → 共线使用的其他原料（同成品的全部原料一并纳入）
	if len(finIDs) > 0 {
		rr, _ := tx.Query(ctx,
			`SELECT DISTINCT raw_batch_id FROM batch_links WHERE finished_batch_id=ANY($1::bigint[])`, finIDs)
		for rr.Next() {
			var id int64
			_ = rr.Scan(&id)
			rawIDs = appendUnique(rawIDs, id)
		}
		rr.Close()
	}
	if len(finIDs) == 0 {
		writeErr(w, 400, "事件未关联任何成品批次，无法发起召回（可先补录关联）")
		return
	}

	recallNo := docNo("RC")
	var recallID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO recalls(recall_no,event_id,level,reason,initiated_by)
		VALUES($1,$2,$3,$4,$5) RETURNING id`,
		recallNo, eventID, req.Level, req.Reason, u.ID).Scan(&recallID); err != nil {
		writeErr(w, 400, "召回单创建失败："+err.Error())
		return
	}

	// 3) 锁定原料/成品批次
	_, _ = tx.Exec(ctx, `UPDATE raw_material_batches SET status='recalled' WHERE id=ANY($1::bigint[])`, rawIDs)
	_, _ = tx.Exec(ctx, `UPDATE finished_batches SET status='recalled' WHERE id=ANY($1::bigint[])`, finIDs)
	_, _ = tx.Exec(ctx, `UPDATE quality_events SET status='recalled' WHERE id=$1`, eventID)

	// 4) 已发货渠道/发货单停售，已售订单标记召回
	_, _ = tx.Exec(ctx, `UPDATE shipments SET status='recalled'
		WHERE id IN (SELECT shipment_id FROM shipment_items WHERE finished_batch_id=ANY($1::bigint[]))`, finIDs)

	type ordInfo struct {
		ID            int64
		OrderNo       string
		Channel       string
		BoxNo         string
		Qty           float64
		Customer      *string
		FinishedBatch string
	}
	var orders []ordInfo
	orows, _ := tx.Query(ctx, `
		SELECT o.id,o.order_no,COALESCE((SELECT name FROM channels WHERE id=o.channel_id),'未知渠道'),
		       o.box_no,o.qty,o.customer,f.batch_no
		FROM orders o
		JOIN finished_batches f ON f.id=o.finished_batch_id
		WHERE o.finished_batch_id=ANY($1::bigint[]) AND o.status IN ('sold','returned')`, finIDs)
	for orows.Next() {
		var o ordInfo
		_ = orows.Scan(&o.ID, &o.OrderNo, &o.Channel, &o.BoxNo, &o.Qty, &o.Customer, &o.FinishedBatch)
		orders = append(orders, o)
	}
	orows.Close()
	_, _ = tx.Exec(ctx, `UPDATE orders SET status='recalled'
		WHERE finished_batch_id=ANY($1::bigint[]) AND status IN ('sold','returned')`, finIDs)

	// 5) 渠道维度停售通知材料
	channelSet := map[string]bool{}
	var shipChannels []struct {
		ShipNo, Channel, CType, FBatch string
		Boxes                          int
		Qty                            float64
	}
	srows, _ := tx.Query(ctx, `
		SELECT sh.ship_no,COALESCE(ch.name,'未知'),COALESCE(ch.ctype,''),
		       f.batch_no,si.boxes,si.qty_kg
		FROM shipment_items si
		JOIN shipments sh ON sh.id=si.shipment_id
		LEFT JOIN channels ch ON ch.id=sh.channel_id
		JOIN finished_batches f ON f.id=si.finished_batch_id
		WHERE si.finished_batch_id=ANY($1::bigint[])`, finIDs)
	for srows.Next() {
		var it struct {
			ShipNo, Channel, CType, FBatch string
			Boxes                          int
			Qty                            float64
		}
		_ = srows.Scan(&it.ShipNo, &it.Channel, &it.CType, &it.FBatch, &it.Boxes, &it.Qty)
		shipChannels = append(shipChannels, it)
		channelSet[it.Channel] = true
	}
	srows.Close()

	for ch := range channelSet {
		doc := docNo("DOC-STOP")
		_, _ = tx.Exec(ctx, `
			INSERT INTO recall_actions(recall_id,atype,doc_no,title,content)
			VALUES($1,'stop_sale',$2,$3,$4)`,
			recallID, doc, "停售通知-"+ch,
			fmt.Sprintf("致 %s：依据召回单 %s（%s），即日起对涉事成品批次立即停止销售、就地封存，并配合清点库存与退货。",
				ch, recallNo, req.Reason))
	}
	for _, it := range shipChannels {
		doc := docNo("DOC-RET")
		_, _ = tx.Exec(ctx, `
			INSERT INTO recall_actions(recall_id,atype,doc_no,title,content,amount)
			VALUES($1,'return',$2,$3,$4,$5)`,
			recallID, doc, "退货指令-"+it.ShipNo,
			fmt.Sprintf("要求 %s 发货单 %s 退回成品 %s：%d 箱 / %.2fkg，退回原箱并附冷链温度记录。",
				it.Channel, it.ShipNo, it.FBatch, it.Boxes, it.Qty),
			it.Qty)
	}

	// 6) 已售订单逐单赔偿材料 + 客服赔付台账（按 59.9 元/盒预核）
	compTotal := 0.0
	const unitPrice = 59.9
	for _, o := range orders {
		amount := math.Round(o.Qty*unitPrice*100) / 100
		compTotal += amount
		doc := docNo("DOC-COMP")
		_, _ = tx.Exec(ctx, `
			INSERT INTO recall_actions(recall_id,atype,doc_no,title,content,amount)
			VALUES($1,'compensation',$2,$3,$4,$5)`,
			recallID, doc, "消费者赔偿-"+o.OrderNo,
			fmt.Sprintf("订单 %s（渠道 %s，箱号 %s，顾客 %v，成品 %s）召回退赔，数量 %.0f，预核金额 %.2f 元。",
				o.OrderNo, o.Channel, o.BoxNo, o.Customer, o.FinishedBatch, o.Qty, amount),
			amount)
		_, _ = tx.Exec(ctx, `
			INSERT INTO customer_compensations(comp_no,order_id,amount,reason,status)
			VALUES($1,$2,$3,$4,'pending')
			ON CONFLICT DO NOTHING`,
			docNo("COMP"), o.ID, amount, "召回 "+recallNo+" 退赔")
	}

	// 7) 在库成品转隔离
	_, _ = tx.Exec(ctx, `
		UPDATE inventory SET status='quarantined',updated_at=now()
		WHERE finished_batch_id=ANY($1::bigint[]) AND status='available'`, finIDs)
	var stockKg float64
	_ = tx.QueryRow(ctx, `
		SELECT COALESCE(sum(qty_kg),0) FROM inventory
		WHERE finished_batch_id=ANY($1::bigint[]) AND status='quarantined'`, finIDs).Scan(&stockKg)

	// 8) 留样检测结果汇总（召回判定依据）
	var qualified, unqualified, pendingSamples int
	_ = tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE result='qualified'),
		       count(*) FILTER (WHERE result='unqualified'),
		       count(*) FILTER (WHERE result='pending')
		FROM samples WHERE finished_batch_id=ANY($1::bigint[])`, finIDs).
		Scan(&qualified, &unqualified, &pendingSamples)

	// 9) 供应商追责材料 + 扣款台账（按原料货值 40 元/kg + 连带赔偿测算）
	supRows, _ := tx.Query(ctx, `
		SELECT DISTINCT sp.id,sp.code,sp.name,rm.batch_no
		FROM raw_material_batches rm JOIN suppliers sp ON sp.id=rm.supplier_id
		WHERE rm.id=ANY($1::bigint[])`, rawIDs)
	type supInfo struct {
		ID                int64
		Code, Name, Batch string
	}
	var sups []supInfo
	for supRows.Next() {
		var s supInfo
		_ = supRows.Scan(&s.ID, &s.Code, &s.Name, &s.Batch)
		sups = append(sups, s)
	}
	supRows.Close()

	var rawKg float64
	_ = tx.QueryRow(ctx, `SELECT COALESCE(sum(qty_kg),0) FROM raw_material_batches WHERE id=ANY($1::bigint[])`, rawIDs).Scan(&rawKg)
	for _, sp := range sups {
		claimAmount := math.Round((rawKg*40+compTotal)*100) / 100
		doc := docNo("DOC-CLM")
		_, _ = tx.Exec(ctx, `
			INSERT INTO recall_actions(recall_id,atype,doc_no,title,content,amount)
			VALUES($1,'supplier_claim',$2,$3,$4,$5)`,
			recallID, doc, "供应商追责-"+sp.Code,
			fmt.Sprintf("向供应商 %s（%s）就原料批次 %s 发起追责：原料货值及连带赔偿合计 %.2f 元（含消费者预核赔付 %.2f 元），附检疫证明、入库温控与召回记录。",
				sp.Name, sp.Code, sp.Batch, claimAmount, compTotal),
			claimAmount)
		dn := docNo("DED")
		_, _ = tx.Exec(ctx, `
			INSERT INTO supplier_deductions(deduction_no,supplier_id,recall_id,raw_batch_id,amount,reason,status)
			SELECT $1,supplier_id,$2,id,$3,$4,'pending' FROM raw_material_batches WHERE batch_no=$5`,
			dn, recallID, claimAmount, "召回 "+recallNo+" 追责扣款", sp.Batch)
	}

	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"recall_no": recallNo, "id": recallID,
		"affected_raw": len(rawIDs), "affected_finished": len(finIDs),
		"affected_orders": len(orders), "quarantined_kg": stockKg,
		"samples":            map[string]any{"qualified": qualified, "unqualified": unqualified, "pending": pendingSamples},
		"compensation_total": math.Round(compTotal*100) / 100,
	})
}

func appendUnique(s []int64, v int64) []int64 {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func (s *Server) listRecalls(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT rc.id,rc.recall_no,e.event_no,rc.level,rc.reason,rc.status,
		       rc.initiated_at::text,
		       (SELECT count(*) FROM recall_actions a WHERE a.recall_id=rc.id) AS docs
		FROM recalls rc JOIN quality_events e ON e.id=rc.event_id
		ORDER BY rc.initiated_at DESC`)
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

func (s *Server) getRecall(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	ctx := r.Context()

	var recallNo, level, reason, status, initiatedAt, eventNo, etype string
	err := s.pool.QueryRow(ctx, `
		SELECT rc.recall_no,rc.level,rc.reason,rc.status,rc.initiated_at::text,e.event_no,e.etype
		FROM recalls rc JOIN quality_events e ON e.id=rc.event_id WHERE rc.id=$1`, id).
		Scan(&recallNo, &level, &reason, &status, &initiatedAt, &eventNo, &etype)
	if err != nil {
		writeErr(w, 404, "召回单不存在")
		return
	}
	head := map[string]any{
		"recall_no": recallNo, "level": level, "reason": reason, "status": status,
		"initiated_at": initiatedAt, "event_no": eventNo, "event_type": etype,
	}

	qm := func(sql string, args ...any) []map[string]any {
		rows, qerr := s.pool.Query(ctx, sql, args...)
		if qerr != nil {
			return []map[string]any{}
		}
		defer rows.Close()
		return scanMaps(rows)
	}

	raws := qm(`
		SELECT rm.batch_no,sp.name AS supplier,rm.species,rm.inbound_temp_c,
		       rm.box_no,rm.location,rm.qty_kg,rm.status
		FROM raw_material_batches rm
		JOIN batch_links l ON l.raw_batch_id=rm.id
		JOIN finished_batches f ON f.id=l.finished_batch_id
		JOIN quality_events e ON e.id=(SELECT event_id FROM recalls WHERE id=$1)
		LEFT JOIN suppliers sp ON sp.id=rm.supplier_id
		WHERE f.id IN (
		  SELECT ff.id FROM finished_batches ff
		  JOIN batch_links ll ON ll.finished_batch_id=ff.id
		  WHERE ll.raw_batch_id=e.raw_batch_id OR ff.id=e.finished_batch_id
		)
		GROUP BY rm.batch_no,sp.name,rm.species,rm.inbound_temp_c,rm.box_no,rm.location,rm.qty_kg,rm.status`, id)

	finished := qm(`
		SELECT DISTINCT f.batch_no,f.product_name,f.packaging_spec,f.qty_kg,f.status
		FROM finished_batches f
		JOIN batch_links l ON l.finished_batch_id=f.id
		JOIN quality_events e ON e.id=(SELECT event_id FROM recalls WHERE id=$1)
		WHERE l.raw_batch_id=e.raw_batch_id OR f.id=e.finished_batch_id`, id)

	shipments := qm(`
		SELECT DISTINCT sh.ship_no,ch.name AS channel,sh.status,sh.shipped_at::text
		FROM shipments sh
		JOIN shipment_items si ON si.shipment_id=sh.id
		LEFT JOIN channels ch ON ch.id=sh.channel_id
		JOIN quality_events e ON e.id=(SELECT event_id FROM recalls WHERE id=$1)
		WHERE si.finished_batch_id=e.finished_batch_id
		   OR si.finished_batch_id IN (SELECT finished_batch_id FROM batch_links WHERE raw_batch_id=e.raw_batch_id)`, id)

	orders := qm(`
		SELECT o.order_no,ch.name AS channel,f.batch_no AS finished_batch_no,
		       o.box_no,o.qty,o.customer,o.status
		FROM orders o LEFT JOIN channels ch ON ch.id=o.channel_id
		JOIN finished_batches f ON f.id=o.finished_batch_id
		JOIN quality_events e ON e.id=(SELECT event_id FROM recalls WHERE id=$1)
		WHERE f.id=e.finished_batch_id
		   OR f.id IN (SELECT finished_batch_id FROM batch_links WHERE raw_batch_id=e.raw_batch_id)`, id)

	inventory := qm(`
		SELECT f.batch_no,i.location,i.qty_kg,i.status
		FROM inventory i JOIN finished_batches f ON f.id=i.finished_batch_id
		JOIN quality_events e ON e.id=(SELECT event_id FROM recalls WHERE id=$1)
		WHERE f.id=e.finished_batch_id
		   OR f.id IN (SELECT finished_batch_id FROM batch_links WHERE raw_batch_id=e.raw_batch_id)`, id)

	samples := qm(`
		SELECT f.batch_no AS finished_batch_no,sm.sample_no,sm.result,sm.lab_note,sm.tested_at::text
		FROM samples sm JOIN finished_batches f ON f.id=sm.finished_batch_id
		JOIN quality_events e ON e.id=(SELECT event_id FROM recalls WHERE id=$1)
		WHERE f.id=e.finished_batch_id
		   OR f.id IN (SELECT finished_batch_id FROM batch_links WHERE raw_batch_id=e.raw_batch_id)`, id)

	actions := qm(`
		SELECT atype,doc_no,title,content,amount,status,created_at::text
		FROM recall_actions WHERE recall_id=$1 ORDER BY
		CASE atype WHEN 'stop_sale' THEN 1 WHEN 'return' THEN 2
		           WHEN 'compensation' THEN 3 WHEN 'supplier_claim' THEN 4 END,doc_no`, id)

	returns := qm(`
		SELECT rg.return_no,f.batch_no AS finished_batch_no,rg.box_no,rg.qty_kg,
		       rg.disposition,rg.arrived_at::text,rg.note
		FROM return_goods rg JOIN finished_batches f ON f.id=rg.finished_batch_id
		WHERE rg.recall_id=$1 ORDER BY rg.arrived_at DESC`, id)

	deductions := qm(`
		SELECT d.deduction_no,sp.name AS supplier,rm.batch_no AS raw_batch_no,
		       d.amount,d.reason,d.status
		FROM supplier_deductions d
		LEFT JOIN suppliers sp ON sp.id=d.supplier_id
		LEFT JOIN raw_material_batches rm ON rm.id=d.raw_batch_id
		WHERE d.recall_id=$1`, id)

	writeJSON(w, 200, map[string]any{
		"recall": head, "raw_batches": raws, "finished_batches": finished,
		"shipments": shipments, "orders": orders, "inventory": inventory,
		"samples": samples, "actions": actions, "returns": returns,
		"deductions": deductions,
	})
}

func (s *Server) closeRecall(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	tag, err := s.pool.Exec(r.Context(), `UPDATE recalls SET status='closed' WHERE id=$1`, id)
	if err != nil || tag.RowsAffected() == 0 {
		writeErr(w, 404, "召回单不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": "closed"})
}
