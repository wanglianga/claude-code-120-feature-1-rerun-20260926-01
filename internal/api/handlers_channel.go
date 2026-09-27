package api

import (
	"net/http"
	"strings"
)

type shipmentReq struct {
	ShipNo      string            `json:"ship_no"`
	ChannelCode string            `json:"channel_code"`
	Items       []shipmentItemReq `json:"items"`
}

type shipmentItemReq struct {
	FinishedBatchNo string  `json:"finished_batch_no"`
	Boxes           int     `json:"boxes"`
	QtyKg           float64 `json:"qty_kg"`
}

func (s *Server) createShipment(w http.ResponseWriter, r *http.Request) {
	var req shipmentReq
	if err := decode(r, &req); err != nil || req.ShipNo == "" || len(req.Items) == 0 {
		writeErr(w, 400, "缺少 ship_no/items")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	var shipID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO shipments(ship_no,channel_id)
		SELECT $1,id FROM channels WHERE code=$2 RETURNING id`,
		req.ShipNo, req.ChannelCode).Scan(&shipID); err != nil {
		writeErr(w, 400, "发货单创建失败："+err.Error())
		return
	}
	for _, it := range req.Items {
		var finID int64
		var labelMark, restrict string
		if err := tx.QueryRow(ctx,
			`SELECT id,COALESCE(label_mark,''),COALESCE(channel_restriction,'')
			 FROM finished_batches WHERE batch_no=$1`, it.FinishedBatchNo).
			Scan(&finID, &labelMark, &restrict); err != nil {
			writeErr(w, 404, "成品批次不存在："+it.FinishedBatchNo)
			return
		}
		// 降级品限定渠道发货：仅允许发往复测结论指定的渠道（默认仅餐饮经销 CH04）
		if labelMark == "downgraded" {
			allowed := map[string]bool{}
			for _, c := range strings.Split(restrict, ",") {
				allowed[strings.TrimSpace(c)] = true
			}
			if !allowed[req.ChannelCode] {
				writeErr(w, 409, "降级品 "+it.FinishedBatchNo+" 仅限渠道："+
					s.channelNames(ctx, restrict)+"，不得发往 "+req.ChannelCode)
				return
			}
		}
		// 从可用库存扣减
		tag, err := tx.Exec(ctx, `
			UPDATE inventory SET qty_kg=qty_kg-$2, updated_at=now()
			WHERE finished_batch_id=$1 AND status='available' AND qty_kg>=$2`,
			finID, it.QtyKg)
		if err != nil || tag.RowsAffected() == 0 {
			writeErr(w, 409, "可用冷库库存不足，无法发货 "+it.FinishedBatchNo)
			return
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO shipment_items(shipment_id,finished_batch_id,boxes,qty_kg)
			VALUES($1,$2,$3,$4)`, shipID, finID, it.Boxes, it.QtyKg); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"shipment_id": shipID, "ship_no": req.ShipNo})
}

func (s *Server) listShipments(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT sh.id,sh.ship_no,ch.code AS channel_code,ch.name AS channel,ch.ctype,
		       sh.shipped_at::text,sh.status,
		       COALESCE(sum(si.qty_kg),0) AS qty_kg,
		       COALESCE(sum(si.boxes),0) AS boxes
		FROM shipments sh LEFT JOIN channels ch ON ch.id=sh.channel_id
		LEFT JOIN shipment_items si ON si.shipment_id=sh.id
		GROUP BY sh.id,ch.code,ch.name,ch.ctype
		ORDER BY sh.shipped_at DESC`)
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

type orderReq struct {
	OrderNo         string  `json:"order_no"`
	ShipNo          string  `json:"ship_no"`
	FinishedBatchNo string  `json:"finished_batch_no"`
	ChannelCode     string  `json:"channel_code"`
	BoxNo           string  `json:"box_no"`
	Qty             float64 `json:"qty"`
	Customer        string  `json:"customer"`
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	var req orderReq
	if err := decode(r, &req); err != nil || req.OrderNo == "" || req.BoxNo == "" {
		writeErr(w, 400, "缺少 order_no/box_no")
		return
	}
	var id int64
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO orders(order_no,shipment_id,finished_batch_id,channel_id,box_no,qty,customer)
		SELECT $1,
		  (SELECT id FROM shipments WHERE ship_no=$2),
		  (SELECT id FROM finished_batches WHERE batch_no=$3),
		  (SELECT id FROM channels WHERE code=$4),
		  $5,$6,$7
		RETURNING id`,
		req.OrderNo, req.ShipNo, req.FinishedBatchNo, req.ChannelCode,
		req.BoxNo, req.Qty, req.Customer).Scan(&id)
	if err != nil {
		writeErr(w, 400, "订单创建失败："+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "order_no": req.OrderNo})
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT o.id,o.order_no,sh.ship_no,f.batch_no AS finished_batch_no,
		       ch.name AS channel,o.box_no,o.qty,o.customer,o.status,o.sold_at::text
		FROM orders o
		LEFT JOIN shipments sh ON sh.id=o.shipment_id
		LEFT JOIN finished_batches f ON f.id=o.finished_batch_id
		LEFT JOIN channels ch ON ch.id=o.channel_id
		ORDER BY o.sold_at DESC LIMIT 500`)
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

type feedbackReq struct {
	ChannelCode string `json:"channel_code"`
	OrderNo     string `json:"order_no"`
	FType       string `json:"ftype"`
	Content     string `json:"content"`
}

func (s *Server) createFeedback(w http.ResponseWriter, r *http.Request) {
	var req feedbackReq
	if err := decode(r, &req); err != nil || req.FType == "" {
		writeErr(w, 400, "缺少 ftype")
		return
	}
	u := currentUser(r)
	ctx := r.Context()
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO channel_feedback(channel_id,order_id,finished_batch_id,ftype,content)
		SELECT (SELECT id FROM channels WHERE code=$1),
		       (SELECT id FROM orders WHERE order_no=$2),
		       (SELECT finished_batch_id FROM orders WHERE order_no=$2),
		       $3,$4
		RETURNING id`,
		req.ChannelCode, req.OrderNo, req.FType, req.Content).Scan(&id)
	if err != nil {
		writeErr(w, 400, "反馈登记失败："+err.Error())
		return
	}
	// 异味/直播集中退货 反馈自动生成品控事件（直播退货为 critical）
	etype, sev := "", "major"
	switch req.FType {
	case "odor":
		etype, sev = "channel_odor", "major"
	case "live_return":
		etype, sev = "live_return", "critical"
	}
	evNo := ""
	if etype != "" {
		evNo = docNo("EV")
		_, _ = s.pool.Exec(ctx, `
			INSERT INTO quality_events(event_no,etype,severity,order_id,finished_batch_id,description,status,created_by)
			SELECT $1,$2,$3,id,finished_batch_id,$4,'open',$5
			FROM channel_feedback WHERE id=$6`,
			evNo, etype, sev, "渠道反馈："+req.Content, u.ID, id)
	}
	writeJSON(w, 201, map[string]any{"id": id, "auto_event_no": evNo})
}

func (s *Server) listFeedback(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT fb.id,ch.name AS channel,o.order_no,f.batch_no AS finished_batch_no,
		       fb.ftype,fb.content,fb.received_at::text,fb.handled
		FROM channel_feedback fb
		LEFT JOIN channels ch ON ch.id=fb.channel_id
		LEFT JOIN orders o ON o.id=fb.order_id
		LEFT JOIN finished_batches f ON f.id=fb.finished_batch_id
		ORDER BY fb.received_at DESC`)
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}
