package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// ---------- 退货回厂：可复检 / 可返工 / 必须销毁 ----------

func (s *Server) listReturns(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT rg.id,rg.return_no,rc.recall_no,f.batch_no AS finished_batch_no,
		       rg.box_no,rg.qty_kg,rg.arrived_at::text,rg.disposition,
		       rg.decided_by,rg.decided_at::text,rg.note
		FROM return_goods rg
		LEFT JOIN recalls rc ON rc.id=rg.recall_id
		LEFT JOIN finished_batches f ON f.id=rg.finished_batch_id
		ORDER BY rg.arrived_at DESC`)
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

type returnReq struct {
	RecallNo        string  `json:"recall_no"`
	FinishedBatchNo string  `json:"finished_batch_no"`
	BoxNo           string  `json:"box_no"`
	QtyKg           float64 `json:"qty_kg"`
	Note            string  `json:"note"`
}

func (s *Server) createReturn(w http.ResponseWriter, r *http.Request) {
	var req returnReq
	if err := decode(r, &req); err != nil || req.FinishedBatchNo == "" {
		writeErr(w, 400, "缺少 finished_batch_no")
		return
	}
	no := docNo("RT")
	var id int64
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO return_goods(return_no,recall_id,finished_batch_id,box_no,qty_kg,note)
		SELECT $1,(SELECT id FROM recalls WHERE recall_no=$2),
		       (SELECT id FROM finished_batches WHERE batch_no=$3),$4,$5,$6
		RETURNING id`,
		no, req.RecallNo, req.FinishedBatchNo, req.BoxNo, req.QtyKg, req.Note).Scan(&id)
	if err != nil {
		writeErr(w, 400, "退货登记失败："+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "return_no": no, "await_disposition": true})
}

type disposeReq struct {
	Disposition string `json:"disposition"` // reinspect / rework / destroy
	Note        string `json:"note"`
}

// disposeReturn 三态处置并同步库存，避免召回后库存状态失真：
// reinspect → 隔离待判库存；rework → 库存转 reworked，成品批次转 reworked；
// destroy → 库存转 destroyed，成品批次转 destroyed。
// 渠道退回为新增到厂货物，不冲减召回时已隔离的厂内原库存。
func (s *Server) disposeReturn(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req disposeReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "字段错误")
		return
	}
	if req.Disposition != "reinspect" && req.Disposition != "rework" && req.Disposition != "destroy" {
		writeErr(w, 400, "disposition 必须为 reinspect/rework/destroy")
		return
	}
	u := currentUser(r)
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	var finID int64
	var qty float64
	var existing *string
	if err := tx.QueryRow(ctx,
		`SELECT finished_batch_id,qty_kg,disposition FROM return_goods WHERE id=$1 FOR UPDATE`, id).
		Scan(&finID, &qty, &existing); err != nil {
		writeErr(w, 404, "退货记录不存在")
		return
	}
	if existing != nil {
		writeErr(w, 409, "该退货已处置："+*existing)
		return
	}

	if _, err := tx.Exec(ctx, `
		UPDATE return_goods SET disposition=$2,decided_by=$3,decided_at=now(),note=COALESCE(NULLIF($4,''),note)
		WHERE id=$1`, id, req.Disposition, u.Username, req.Note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	var loc string
	_ = tx.QueryRow(ctx, `SELECT COALESCE(cold_storage_location,'冷库待判区') FROM finished_batches WHERE id=$1`, finID).Scan(&loc)

	// 渠道退回是“新增到厂货物”，不扣减召回时已隔离的厂内原库存；
	// 直接按处置结论进入独立状态桶，保证可用/隔离/返工/销毁账实一致。
	switch req.Disposition {
	case "reinspect":
		// 维持隔离，等待复检；单列一条隔离库存便于盘点
		_, _ = tx.Exec(ctx, `
			INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
			VALUES($1,$2,$3,'quarantined')
			ON CONFLICT (finished_batch_id,location,status)
			DO UPDATE SET qty_kg=inventory.qty_kg+EXCLUDED.qty_kg,updated_at=now()`,
			finID, loc+"-复检待判", qty)
	case "rework":
		_, _ = tx.Exec(ctx, `
			INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
			VALUES($1,$2,$3,'reworked')
			ON CONFLICT (finished_batch_id,location,status)
			DO UPDATE SET qty_kg=inventory.qty_kg+EXCLUDED.qty_kg,updated_at=now()`,
			finID, loc+"-返工区", qty)
		_, _ = tx.Exec(ctx, `UPDATE finished_batches SET status='reworked' WHERE id=$1`, finID)
	case "destroy":
		_, _ = tx.Exec(ctx, `
			INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
			VALUES($1,$2,$3,'destroyed')
			ON CONFLICT (finished_batch_id,location,status)
			DO UPDATE SET qty_kg=inventory.qty_kg+EXCLUDED.qty_kg,updated_at=now()`,
			finID, loc+"-销毁区", qty)
		_, _ = tx.Exec(ctx, `UPDATE finished_batches SET status='destroyed' WHERE id=$1`, finID)
	}

	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	label := map[string]string{"reinspect": "可复检-隔离待判", "rework": "可返工", "destroy": "必须销毁"}
	writeJSON(w, 200, map[string]any{"id": id, "disposition": req.Disposition, "label": label[req.Disposition]})
}

// ---------- 库存 / 盘点 ----------

func (s *Server) listInventory(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT i.id,f.batch_no AS finished_batch_no,f.product_name,i.location,
		       i.qty_kg,i.status,i.updated_at::text
		FROM inventory i JOIN finished_batches f ON f.id=i.finished_batch_id
		ORDER BY i.status,i.location`)
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

func (s *Server) listStocktakes(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT st.id,st.stocktake_no,st.location,st.counted_at::text,st.operator,st.note
		FROM stocktakes st ORDER BY st.counted_at DESC LIMIT 100`)
	defer rows.Close()
	out := scanMaps(rows)
	writeJSON(w, 200, out)
}

type stocktakeReq struct {
	Location string `json:"location"`
	Operator string `json:"operator"`
	Note     string `json:"note"`
}

// createStocktake 冷库盘点：按库位快照系统库存，盘点数量默认等于系统量，
// 差异写 diff_qty（可随后在库内更正）；返回逐条差异。
func (s *Server) createStocktake(w http.ResponseWriter, r *http.Request) {
	var req stocktakeReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "字段错误")
		return
	}
	u := currentUser(r)
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	no := docNo("ST")
	var stID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO stocktakes(stocktake_no,location,operator,note)
		VALUES($1,$2,$3,$4) RETURNING id`,
		no, req.Location, firstNonEmpty(req.Operator, u.Username), req.Note).Scan(&stID); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rows, _ := tx.Query(ctx, `
		SELECT finished_batch_id,sum(qty_kg)
		FROM inventory WHERE ($1='' OR location=$1)
		GROUP BY finished_batch_id`, req.Location)
	type it struct {
		fid int64
		qty float64
	}
	var items []it
	for rows.Next() {
		var x it
		_ = rows.Scan(&x.fid, &x.qty)
		items = append(items, x)
	}
	rows.Close()
	for _, x := range items {
		_, _ = tx.Exec(ctx, `
			INSERT INTO stocktake_items(stocktake_id,finished_batch_id,system_qty,counted_qty,diff_qty)
			VALUES($1,$2,$3,$3,0)`, stID, x.fid, x.qty)
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 盘点单明细
	drows, _ := s.pool.Query(ctx, `
		SELECT f.batch_no AS finished_batch_no,si.system_qty,si.counted_qty,si.diff_qty
		FROM stocktake_items si JOIN finished_batches f ON f.id=si.finished_batch_id
		WHERE si.stocktake_id=$1`, stID)
	defer drows.Close()
	writeJSON(w, 201, map[string]any{
		"stocktake_no": no, "items": scanMaps(drows), "count": len(items),
	})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---------- 供应商扣款 ----------

func (s *Server) listDeductions(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT d.id,d.deduction_no,sp.code AS supplier_code,sp.name AS supplier,
		       rm.batch_no AS raw_batch_no,rc.recall_no,d.amount,d.reason,d.status,d.created_at::text
		FROM supplier_deductions d
		LEFT JOIN suppliers sp ON sp.id=d.supplier_id
		LEFT JOIN raw_material_batches rm ON rm.id=d.raw_batch_id
		LEFT JOIN recalls rc ON rc.id=d.recall_id
		ORDER BY d.created_at DESC`)
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

type deductionReq struct {
	SupplierCode string  `json:"supplier_code"`
	RawBatchNo   string  `json:"raw_batch_no"`
	RecallNo     string  `json:"recall_no"`
	Amount       float64 `json:"amount"`
	Reason       string  `json:"reason"`
}

func (s *Server) createDeduction(w http.ResponseWriter, r *http.Request) {
	var req deductionReq
	if err := decode(r, &req); err != nil || req.Amount <= 0 {
		writeErr(w, 400, "缺少/金额非法")
		return
	}
	no := docNo("DED")
	var id int64
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO supplier_deductions(deduction_no,supplier_id,recall_id,raw_batch_id,amount,reason)
		SELECT $1,(SELECT id FROM suppliers WHERE code=$2),
		       (SELECT id FROM recalls WHERE recall_no=$3),
		       (SELECT id FROM raw_material_batches WHERE batch_no=$4),$5,$6
		RETURNING id`,
		no, req.SupplierCode, req.RecallNo, req.RawBatchNo, req.Amount, req.Reason).Scan(&id)
	if err != nil {
		writeErr(w, 400, "扣款单创建失败："+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "deduction_no": no, "status": "pending"})
}

type dedStatusReq struct {
	Status string `json:"status"`
}

func (s *Server) updateDeduction(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req dedStatusReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "字段错误")
		return
	}
	if req.Status != "confirmed" && req.Status != "deducted" && req.Status != "pending" {
		writeErr(w, 400, "status 必须为 pending/confirmed/deducted")
		return
	}
	tag, err := s.pool.Exec(r.Context(),
		`UPDATE supplier_deductions SET status=$2 WHERE id=$1`, id, req.Status)
	if err != nil || tag.RowsAffected() == 0 {
		writeErr(w, 404, "扣款单不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": req.Status})
}

// ---------- 客服赔付 ----------

func (s *Server) listCompensations(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT c.id,c.comp_no,o.order_no,ch.name AS channel,f.batch_no AS finished_batch_no,
		       c.amount,c.reason,c.status,c.created_at::text
		FROM customer_compensations c
		LEFT JOIN orders o ON o.id=c.order_id
		LEFT JOIN channels ch ON ch.id=o.channel_id
		LEFT JOIN finished_batches f ON f.id=o.finished_batch_id
		ORDER BY c.created_at DESC`)
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

type compReq struct {
	OrderNo string  `json:"order_no"`
	Amount  float64 `json:"amount"`
	Reason  string  `json:"reason"`
}

func (s *Server) createCompensation(w http.ResponseWriter, r *http.Request) {
	var req compReq
	if err := decode(r, &req); err != nil || req.Amount <= 0 {
		writeErr(w, 400, "缺少/金额非法")
		return
	}
	no := docNo("COMP")
	var id int64
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO customer_compensations(comp_no,order_id,amount,reason)
		SELECT $1,id,$2,$3 FROM orders WHERE order_no=$4 RETURNING id`,
		no, req.Amount, req.Reason, req.OrderNo).Scan(&id)
	if err != nil {
		writeErr(w, 400, "赔付单创建失败："+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "comp_no": no})
}

func (s *Server) payCompensation(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	tag, err := s.pool.Exec(r.Context(),
		`UPDATE customer_compensations SET status='paid' WHERE id=$1 AND status='pending'`, id)
	if err != nil || tag.RowsAffected() == 0 {
		writeErr(w, 404, "赔付单不存在或已支付")
		return
	}
	_, _ = s.pool.Exec(r.Context(),
		`UPDATE orders SET status='compensated' WHERE id=(SELECT order_id FROM customer_compensations WHERE id=$1) AND status='recalled'`, id)
	writeJSON(w, 200, map[string]any{"id": id, "status": "paid"})
}

// ---------- 监管抽样 ----------

func (s *Server) listSamplings(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.pool.Query(r.Context(), `
		SELECT g.id,g.sampling_no,g.regulator,g.target_type,
		       rm.batch_no AS raw_batch_no,f.batch_no AS finished_batch_no,
		       g.sampled_at::text,g.result,g.note
		FROM regulatory_samplings g
		LEFT JOIN raw_material_batches rm ON rm.id=g.raw_batch_id
		LEFT JOIN finished_batches f ON f.id=g.finished_batch_id
		ORDER BY g.sampled_at DESC`)
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

type samplingReq struct {
	Regulator       string `json:"regulator"`
	TargetType      string `json:"target_type"`
	RawBatchNo      string `json:"raw_batch_no"`
	FinishedBatchNo string `json:"finished_batch_no"`
	Note            string `json:"note"`
}

func (s *Server) createSampling(w http.ResponseWriter, r *http.Request) {
	var req samplingReq
	if err := decode(r, &req); err != nil || req.Regulator == "" ||
		(req.TargetType != "raw" && req.TargetType != "finished") {
		writeErr(w, 400, "regulator 必填，target_type 为 raw/finished")
		return
	}
	no := docNo("GS")
	var id int64
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO regulatory_samplings(sampling_no,regulator,target_type,raw_batch_id,finished_batch_id,note)
		SELECT $1,$2,$3,
		  (SELECT id FROM raw_material_batches WHERE batch_no=$4),
		  (SELECT id FROM finished_batches WHERE batch_no=$5),$6
		RETURNING id`,
		no, req.Regulator, req.TargetType, req.RawBatchNo, req.FinishedBatchNo, req.Note).Scan(&id)
	if err != nil {
		writeErr(w, 400, "抽样登记失败："+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "sampling_no": no})
}

type samplingResultReq struct {
	Result string `json:"result"`
	Note   string `json:"note"`
}

func (s *Server) samplingResult(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req samplingResultReq
	if err := decode(r, &req); err != nil || (req.Result != "qualified" && req.Result != "unqualified") {
		writeErr(w, 400, "result 必须为 qualified/unqualified")
		return
	}
	tag, _ := s.pool.Exec(r.Context(),
		`UPDATE regulatory_samplings SET result=$2,note=COALESCE(NULLIF($3,''),note) WHERE id=$1`,
		id, req.Result, req.Note)
	if tag.RowsAffected() == 0 {
		writeErr(w, 404, "抽样记录不存在")
		return
	}
	// 监管抽检不合格自动生成监管召回事件
	evNo := ""
	if req.Result == "unqualified" {
		evNo = docNo("EV")
		_, _ = s.pool.Exec(r.Context(), `
			INSERT INTO quality_events(event_no,etype,severity,raw_batch_id,finished_batch_id,description,status,created_by)
			SELECT $1,'regulatory_recall','critical',raw_batch_id,finished_batch_id,
			       '监管抽样不合格：'||regulator||' '||COALESCE($2,''),'open',$3
			FROM regulatory_samplings WHERE id=$4`,
			evNo, req.Note, currentUser(r).ID, id)
	}
	writeJSON(w, 200, map[string]any{"id": id, "result": req.Result, "auto_event_no": evNo})
}
