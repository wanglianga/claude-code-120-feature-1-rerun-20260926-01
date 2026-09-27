package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// docNo 生成业务单号：前缀 + 时间戳 + 2 字节随机
func docNo(prefix string) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s%s%s", prefix, time.Now().Format("20060102150405"), hex.EncodeToString(b))
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT p.id,p.plan_no,p.product_name,l.code,l.name,p.target_qty_kg,
		       p.packaging_spec,p.required_seasoning_batch,p.status,p.planned_at::text
		FROM production_plans p LEFT JOIN production_lines l ON l.id=p.line_id
		ORDER BY p.planned_at DESC`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

type planReq struct {
	PlanNo            string  `json:"plan_no"`
	ProductName       string  `json:"product_name"`
	LineCode          string  `json:"line_code"`
	TargetQtyKg       float64 `json:"target_qty_kg"`
	PackagingSpec     string  `json:"packaging_spec"`
	RequiredSeasoning string  `json:"required_seasoning_batch"`
}

func (s *Server) createPlan(w http.ResponseWriter, r *http.Request) {
	var req planReq
	if err := decode(r, &req); err != nil || req.PlanNo == "" || req.ProductName == "" {
		writeErr(w, 400, "缺少 plan_no/product_name 等字段")
		return
	}
	u := currentUser(r)
	var id int64
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO production_plans(plan_no,product_name,line_id,target_qty_kg,
		  packaging_spec,required_seasoning_batch,created_by)
		SELECT $1,$2,id,$3,$4,$5,$6 FROM production_lines WHERE code=$7
		RETURNING id`,
		req.PlanNo, req.ProductName, req.TargetQtyKg, req.PackagingSpec,
		req.RequiredSeasoning, u.ID, req.LineCode).Scan(&id)
	if err != nil {
		writeErr(w, 400, "计划创建失败："+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "plan_no": req.PlanNo})
}

type thawReq struct {
	RawBatchNo  string  `json:"raw_batch_no"`
	StartAt     string  `json:"start_at"`
	EndAt       string  `json:"end_at"`
	CenterTempC float64 `json:"center_temp_c"`
	WaterBatch  string  `json:"water_batch_no"`
	Operator    string  `json:"operator"`
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.FixedZone("CST", 8*3600)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("时间格式应为 RFC3339 或 2006-01-02 15:04:05")
}

func (s *Server) addThaw(w http.ResponseWriter, r *http.Request) {
	planID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req thawReq
	if err := decode(r, &req); err != nil || req.RawBatchNo == "" {
		writeErr(w, 400, "缺少 raw_batch_no 等字段")
		return
	}
	start, err1 := parseTime(req.StartAt)
	end, err2 := parseTime(req.EndAt)
	if err1 != nil || err2 != nil {
		writeErr(w, 400, "时间格式错误")
		return
	}
	if !end.After(start) {
		writeErr(w, 400, "解冻结束时间必须晚于开始时间")
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

	var rawID int64
	var rawStatus string
	if err := tx.QueryRow(ctx,
		`SELECT id,status FROM raw_material_batches WHERE batch_no=$1`, req.RawBatchNo).
		Scan(&rawID, &rawStatus); err != nil {
		writeErr(w, 404, "原料批次不存在："+req.RawBatchNo)
		return
	}
	// 锁批期间暂停该批进入加工线：已存在活动锁批的原料不允许再次解冻/投产
	var activeLock string
	_ = tx.QueryRow(ctx,
		`SELECT lock_no FROM thaw_locks WHERE raw_batch_id=$1 AND status='locked'
		 ORDER BY locked_at DESC LIMIT 1`, rawID).Scan(&activeLock)
	if activeLock != "" || rawStatus == "locked" {
		writeErr(w, 409, fmt.Sprintf(
			"原料批次 %s 已被锁批（%s），暂停进入加工线，须等待品控复测结论", req.RawBatchNo, activeLock))
		return
	}
	if rawStatus == "destroyed" || rawStatus == "recalled" {
		writeErr(w, 409, "原料批次 "+req.RawBatchNo+" 状态为 "+rawStatus+"，不得解冻投产")
		return
	}

	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO thaw_records(plan_id,raw_batch_id,start_at,end_at,center_temp_c,water_batch_no,operator)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		planID, rawID, start, end, req.CenterTempC, req.WaterBatch, req.Operator).Scan(&id); err != nil {
		writeErr(w, 400, "解冻记录失败："+err.Error())
		return
	}
	_, _ = tx.Exec(ctx,
		`UPDATE production_plans SET status='thawing' WHERE id=$1 AND status='planned'`, planID)

	// 解冻超过 4 小时（240 分钟）：自动生成品控事件 + 锁批单，原料立即锁定、退出排程
	timeoutMin := int(end.Sub(start).Minutes())
	alarmEvent := map[string]any{"id": id, "duration_minutes": timeoutMin}
	if timeoutMin > 240 {
		evNo := docNo("EV")
		var eventID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO quality_events(event_no,etype,severity,raw_batch_id,description,status,created_by)
			VALUES($1,'thaw_timeout','major',$2,$3,'open',$4) RETURNING id`,
			evNo, rawID,
			fmt.Sprintf("解冻用时 %d 分钟，超过限值 240 分钟（计划 %d，用水批次 %s），已自动锁批",
				timeoutMin, planID, req.WaterBatch),
			u.ID).Scan(&eventID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		lockNo := docNo("LK")
		if _, err := tx.Exec(ctx, `
			INSERT INTO thaw_locks(lock_no,raw_batch_id,plan_id,thaw_record_id,event_id,
			  duration_minutes,status,locked_by)
			VALUES($1,$2,$3,$4,$5,$6,'locked',$7)`,
			lockNo, rawID, planID, id, eventID, timeoutMin, u.ID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(ctx,
			`UPDATE raw_material_batches SET status='locked' WHERE id=$1`, rawID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		alarmEvent["auto_event_no"] = evNo
		alarmEvent["auto_lock_no"] = lockNo
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if timeoutMin > 240 {
		alarmEvent["locked"] = true
	}
	writeJSON(w, 201, alarmEvent)
}

type processReq struct {
	Stage     string   `json:"stage"`
	Operator  string   `json:"operator"`
	ValueNum  *float64 `json:"value_num"`
	ValueText string   `json:"value_text"`
	OK        *bool    `json:"ok"`
	Note      string   `json:"note"`
}

var stageNames = map[string]string{
	"wash": "清洗", "trim": "切配", "season": "调味", "weigh": "称重",
	"package": "包装", "metalcheck": "金检", "coldstorage": "成品冷库",
}

func (s *Server) addProcess(w http.ResponseWriter, r *http.Request) {
	planID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req processReq
	if err := decode(r, &req); err != nil || stageNames[req.Stage] == "" {
		writeErr(w, 400, "stage 必须是 wash/trim/season/weigh/package/metalcheck/coldstorage")
		return
	}
	ok := true
	if req.OK != nil {
		ok = *req.OK
	}
	u := currentUser(r)
	ctx := r.Context()

	// 锁批期间暂停该批进入加工线：本计划解冻用的原料存在活动锁批时，任何加工工序一律暂停
	var lockNo, lockRaw string
	_ = s.pool.QueryRow(ctx, `
		SELECT l.lock_no, rm.batch_no
		FROM thaw_locks l JOIN raw_material_batches rm ON rm.id=l.raw_batch_id
		WHERE l.status='locked'
		  AND (l.plan_id=$1
		       OR l.raw_batch_id IN (SELECT raw_batch_id FROM thaw_records WHERE plan_id=$1))
		LIMIT 1`, planID).Scan(&lockNo, &lockRaw)
	if lockNo != "" {
		writeErr(w, 409, fmt.Sprintf(
			"原料批次 %s 解冻超时锁批（%s），加工线暂停，须待品控复测并隔离/处置半成品后再继续",
			lockRaw, lockNo))
		return
	}

	var id int64
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO process_records(plan_id,stage,operator,value_num,value_text,ok,note)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		planID, req.Stage, req.Operator, req.ValueNum, req.ValueText, ok, req.Note).Scan(&id); err != nil {
		writeErr(w, 400, "加工记录失败："+err.Error())
		return
	}
	resp := map[string]any{"id": id}

	// 调味料批次用错
	if req.Stage == "season" {
		var required string
		var finID *int64
		_ = s.pool.QueryRow(r.Context(),
			`SELECT COALESCE(required_seasoning_batch,'') FROM production_plans WHERE id=$1`, planID).Scan(&required)
		if required != "" && req.ValueText != required {
			evNo := docNo("EV")
			_ = s.pool.QueryRow(r.Context(), `
				INSERT INTO quality_events(event_no,etype,severity,description,status,created_by)
				VALUES($1,'seasoning_mismatch','critical',$2,'open',$3) RETURNING id`,
				evNo, fmt.Sprintf("计划 %d 调味批次应为 %s，实际投用 %s", planID, required, req.ValueText),
				u.ID).Scan(&finID)
			resp["auto_event_no"] = evNo
		}
	}
	// 金检报警
	if req.Stage == "metalcheck" && !ok {
		evNo := docNo("EV")
		_, _ = s.pool.Exec(r.Context(), `
			INSERT INTO quality_events(event_no,etype,severity,description,status,created_by)
			VALUES($1,'metal_alarm','critical',$2,'open',$3)`,
			evNo, fmt.Sprintf("计划 %d 金检报警：%s", planID, req.Note), u.ID)
		resp["auto_event_no"] = evNo
	}
	// 包装漏气
	if req.Stage == "package" && !ok {
		evNo := docNo("EV")
		_, _ = s.pool.Exec(r.Context(), `
			INSERT INTO quality_events(event_no,etype,severity,description,status,created_by)
			VALUES($1,'package_leak','major',$2,'open',$3)`,
			evNo, fmt.Sprintf("计划 %d 包装检出漏气：%s", planID, req.Note), u.ID)
		resp["auto_event_no"] = evNo
	}
	if req.Stage == "coldstorage" {
		_, _ = s.pool.Exec(r.Context(),
			`UPDATE production_plans SET status='processing' WHERE id=$1`, planID)
	}
	writeJSON(w, 201, resp)
}

type finishReq struct {
	BatchNo        string        `json:"batch_no"`
	PackagingSpec  string        `json:"packaging_spec"`
	QtyKg          float64       `json:"qty_kg"`
	ColdStorageLoc string        `json:"cold_storage_location"`
	Inspector      string        `json:"inspector"`
	SampleResult   string        `json:"sample_result"` // qualified/unqualified/留空 pending
	RawUsages      []rawUsageReq `json:"raw_usages"`
	NormalPrice    float64       `json:"normal_price"`    // 正常品单价 元/kg（降级成本核算用，默认 60）
	DowngradePrice float64       `json:"downgrade_price"` // 降级品销售单价 元/kg（默认 30）
}

type rawUsageReq struct {
	RawBatchNo string  `json:"raw_batch_no"`
	QtyKg      float64 `json:"qty_kg"`
}

func (s *Server) finishPlan(w http.ResponseWriter, r *http.Request) {
	planID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req finishReq
	if err := decode(r, &req); err != nil || req.BatchNo == "" {
		writeErr(w, 400, "缺少 batch_no 等字段")
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

	var productName string
	if err := tx.QueryRow(ctx, `SELECT product_name FROM production_plans WHERE id=$1`, planID).Scan(&productName); err != nil {
		writeErr(w, 404, "生产计划不存在")
		return
	}
	// 确定本批成品耗用的原料（显式传入优先；缺省取本计划解冻记录里的原料）
	var rawIDs []int64
	if len(req.RawUsages) > 0 {
		for _, us := range req.RawUsages {
			var rid int64
			if err := tx.QueryRow(ctx, `SELECT id FROM raw_material_batches WHERE batch_no=$1`,
				us.RawBatchNo).Scan(&rid); err != nil {
				writeErr(w, 404, "原料批次不存在："+us.RawBatchNo)
				return
			}
			rawIDs = appendUnique(rawIDs, rid)
		}
	} else {
		rf, _ := tx.Query(ctx,
			`SELECT DISTINCT raw_batch_id FROM thaw_records
			 WHERE plan_id=$1 AND raw_batch_id IS NOT NULL`, planID)
		for rf.Next() {
			var rid int64
			_ = rf.Scan(&rid)
			rawIDs = appendUnique(rawIDs, rid)
		}
		rf.Close()
	}

	// 锁批期间暂停该批进入加工线：活动锁批未闭环前不得完工
	var activeLock string
	if len(rawIDs) > 0 {
		_ = tx.QueryRow(ctx, `
			SELECT lock_no FROM thaw_locks
			WHERE status='locked' AND (plan_id=$1 OR raw_batch_id=ANY($2::bigint[]))
			LIMIT 1`, planID, rawIDs).Scan(&activeLock)
	}
	if activeLock != "" {
		writeErr(w, 409, fmt.Sprintf(
			"存在解冻超时活动锁批 %s，该原料暂停进入加工线，须待品控复测放行/降级/拒收后再完工", activeLock))
		return
	}
	// 复测拒收销毁 / 已召回的原料不得再完工入库
	var badStatus string
	if len(rawIDs) > 0 {
		_ = tx.QueryRow(ctx, `
			SELECT status FROM raw_material_batches
			WHERE id=ANY($1::bigint[]) AND status IN ('locked','destroyed','recalled')
			LIMIT 1`, rawIDs).Scan(&badStatus)
	}
	if badStatus != "" {
		writeErr(w, 409, "耗用原料存在不可用状态（"+badStatus+"）：锁批待复测/拒收销毁/召回原料不得完工")
		return
	}

	// 降级使用：取最近一张降级锁批的标签与渠道限制，成品继承并单独标记
	labelMark, labelText, channelRestriction := "", "", ""
	var dgLockID *int64
	if len(rawIDs) > 0 {
		_ = tx.QueryRow(ctx, `
			SELECT id,COALESCE(label_text,''),COALESCE(channel_restriction,'')
			FROM thaw_locks
			WHERE status='downgraded' AND raw_batch_id=ANY($1::bigint[])
			ORDER BY closed_at DESC NULLS LAST, retest_at DESC NULLS LAST LIMIT 1`,
			rawIDs).Scan(&dgLockID, &labelText, &channelRestriction)
		if dgLockID != nil {
			labelMark = "downgraded"
		}
	}

	var finID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO finished_batches(batch_no,plan_id,product_name,packaging_spec,qty_kg,
		  cold_storage_location,label_mark,label_text,channel_restriction,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		req.BatchNo, planID, productName, req.PackagingSpec, req.QtyKg, req.ColdStorageLoc,
		labelMark, labelText, channelRestriction, u.ID).Scan(&finID); err != nil {
		writeErr(w, 400, "成品批次创建失败："+err.Error())
		return
	}

	// 原料-成品关联：显式传入优先；缺省取本计划解冻记录里的原料批次
	if len(req.RawUsages) == 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO batch_links(finished_batch_id,raw_batch_id,qty_kg)
			SELECT $1, tr.raw_id, $2 FROM
			  (SELECT DISTINCT raw_batch_id AS raw_id FROM thaw_records
			   WHERE plan_id=$3 AND raw_batch_id IS NOT NULL) tr
			ON CONFLICT DO NOTHING`, finID, req.QtyKg, planID); err != nil {
			writeErr(w, 400, "原料关联失败："+err.Error())
			return
		}
	} else {
		for _, us := range req.RawUsages {
			if _, err := tx.Exec(ctx, `
				INSERT INTO batch_links(finished_batch_id,raw_batch_id,qty_kg)
				SELECT $1,id,$2 FROM raw_material_batches WHERE batch_no=$3
				ON CONFLICT DO NOTHING`, finID, us.QtyKg, us.RawBatchNo); err != nil {
				writeErr(w, 400, "原料关联失败："+err.Error())
				return
			}
		}
	}

	// 成品入库（冷库库存）
	if req.ColdStorageLoc != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
			VALUES($1,$2,$3,'available')
			ON CONFLICT (finished_batch_id,location,status)
			DO UPDATE SET qty_kg=inventory.qty_kg+EXCLUDED.qty_kg, updated_at=now()`,
			finID, req.ColdStorageLoc, req.QtyKg); err != nil {
			writeErr(w, 400, "库存写入失败："+err.Error())
			return
		}
	}

	// 留样
	sampleNo := docNo("SP")
	result := "pending"
	if req.SampleResult == "qualified" || req.SampleResult == "unqualified" {
		result = req.SampleResult
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO samples(sample_no,finished_batch_id,result,tested_at,lab_note,inspector)
		VALUES($1,$2,$3, CASE WHEN $3='pending' THEN NULL ELSE now() END,
		        CASE WHEN $3='pending' THEN '待检留样' ELSE '完工同步检测' END, $4)
		ON CONFLICT (sample_no) DO NOTHING`,
		sampleNo, finID, result, req.Inspector); err != nil {
		writeErr(w, 400, "留样失败："+err.Error())
		return
	}

	if _, err := tx.Exec(ctx, `UPDATE production_plans SET status='finished' WHERE id=$1`, planID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// 降级使用：标签/渠道已随成品单独标记；差价损失进独立成本中心核算
	priceLoss, downgrade := 0.0, false
	if dgLockID != nil {
		downgrade = true
		normalPrice := req.NormalPrice
		if normalPrice <= 0 {
			normalPrice = 60
		}
		dgPrice := req.DowngradePrice
		if dgPrice <= 0 {
			dgPrice = 30
		}
		priceLoss = (normalPrice - dgPrice) * req.QtyKg
		if priceLoss < 0 {
			priceLoss = 0
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO downgrade_costs(cost_no,thaw_lock_id,raw_batch_id,finished_batch_id,
			  qty_kg,normal_price,downgrade_price,price_loss,rework_cost,total_cost,cost_center,note)
			VALUES($1,$2,(SELECT raw_batch_id FROM thaw_locks WHERE id=$2),$3,
			        $4,$5,$6,$7,0,$7,'降级品核算',$8)`,
			docNo("COST"), *dgLockID, finID, req.QtyKg, normalPrice, dgPrice, priceLoss,
			fmt.Sprintf("解冻超时复测降级成品 %s，限定渠道：%s", req.BatchNo, channelRestriction)); err != nil {
			writeErr(w, 400, "降级成本核算失败："+err.Error())
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp := map[string]any{
		"finished_id": finID, "batch_no": req.BatchNo,
		"sample_no": sampleNo, "sample_result": result,
	}
	if downgrade {
		resp["label_mark"] = "downgraded"
		resp["label_text"] = labelText
		resp["channel_restriction"] = channelRestriction
		resp["downgrade_price_loss"] = priceLoss
	}
	writeJSON(w, 201, resp)
}
