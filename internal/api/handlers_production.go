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
		       p.packaging_spec,p.required_seasoning_batch,p.status,p.planned_at::text,
		       rb.batch_no AS locked_raw_batch_no
		FROM production_plans p
		LEFT JOIN production_lines l ON l.id=p.line_id
		LEFT JOIN raw_material_batches rb ON rb.id=p.locked_raw_batch_id
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
	u := currentUser(r)
	var id, rawID int64
	if err := s.pool.QueryRow(r.Context(),
		`SELECT id FROM raw_material_batches WHERE batch_no=$1`, req.RawBatchNo).Scan(&rawID); err != nil {
		writeErr(w, 404, "原料批次不存在："+req.RawBatchNo)
		return
	}
	if err := s.pool.QueryRow(r.Context(), `
		INSERT INTO thaw_records(plan_id,raw_batch_id,start_at,end_at,center_temp_c,water_batch_no,operator)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		planID, rawID, start, end, req.CenterTempC, req.WaterBatch, req.Operator).Scan(&id); err != nil {
		writeErr(w, 400, "解冻记录失败："+err.Error())
		return
	}
	_, _ = s.pool.Exec(r.Context(),
		`UPDATE production_plans SET status='thawing' WHERE id=$1 AND status='planned'`, planID)

	// 解冻超过 4 小时（240 分钟）自动生成品控事件并锁批：
	// 该批原料暂停进入加工线，等待品控复测中心温度与感官状态。
	timeoutMin := int(end.Sub(start).Minutes())
	alarmEvent := map[string]any{"id": id, "duration_minutes": timeoutMin}
	if timeoutMin > 240 {
		evNo := docNo("EV")
		var evID int64
		if err := s.pool.QueryRow(r.Context(), `
			INSERT INTO quality_events(event_no,etype,severity,raw_batch_id,description,status,created_by)
			VALUES($1,'thaw_timeout','major',$2,$3,'open',$4) RETURNING id`,
			evNo, rawID,
			fmt.Sprintf("解冻用时 %d 分钟，超过限值 240 分钟（计划 %d，用水批次 %s），暂停该批进入加工线并锁批待复测",
				timeoutMin, planID, req.WaterBatch),
			u.ID).Scan(&evID); err == nil {
			alarmEvent["auto_event_no"] = evNo
		}
		lockNo := docNo("TL")
		var lockID int64
		if err := s.pool.QueryRow(r.Context(), `
			INSERT INTO thaw_locks(lock_no,event_id,raw_batch_id,plan_id,thaw_record_id,
			  duration_minutes,allowed_minutes,created_by)
			VALUES($1,$2,$3,$4,$5,$6,240,$7) RETURNING id`,
			lockNo, evID, rawID, planID, id, timeoutMin, u.ID).Scan(&lockID); err == nil {
			alarmEvent["thaw_lock_no"] = lockNo
			alarmEvent["locked"] = true
		}
		// 原料锁定（已耗用/已召回/已销毁的不回改状态，锁批档案仍保留）
		_, _ = s.pool.Exec(r.Context(),
			`UPDATE raw_material_batches SET status='locked'
			 WHERE id=$1 AND status IN ('in_storage','locked','downgraded')`, rawID)
		// 暂停该计划进入加工线
		_, _ = s.pool.Exec(r.Context(),
			`UPDATE production_plans SET status='locked',locked_raw_batch_id=$2
			 WHERE id=$1 AND status NOT IN ('finished','cancelled')`, planID, rawID)
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

	// 锁批期间生产排程自动避开该原料：计划处于 locked 时拒绝继续加工，
	// 须先在「解冻锁批」完成复测处置（放行/降级/销毁）。
	var planStatus, lockedRaw string
	_ = s.pool.QueryRow(r.Context(),
		`SELECT COALESCE(p.status,''),COALESCE(rb.batch_no,'')
		 FROM production_plans p LEFT JOIN raw_material_batches rb ON rb.id=p.locked_raw_batch_id
		 WHERE p.id=$1`, planID).Scan(&planStatus, &lockedRaw)
	if planStatus == "locked" {
		writeErr(w, 409, fmt.Sprintf(
			"计划 %d 因原料 %s 解冻超时已锁批暂停，加工线排程自动避开；请先完成品控复测与处置",
			planID, lockedRaw))
		return
	}

	var id int64
	if err := s.pool.QueryRow(r.Context(), `
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

	var productName, planStatus string
	if err := tx.QueryRow(ctx,
		`SELECT product_name,status FROM production_plans WHERE id=$1`, planID).
		Scan(&productName, &planStatus); err != nil {
		writeErr(w, 404, "生产计划不存在")
		return
	}
	if planStatus == "locked" {
		writeErr(w, 409, "计划因解冻超时锁批处于暂停状态，完工被排程拦截；请先在「解冻锁批」完成复测处置")
		return
	}

	downgraded := false
	var dgLabel, dgChannels string
	var lockID int64
	// 判断成品是否使用了降级原料（解冻超时复测降级）：
	// 显式传入原料时逐一核对；缺省取本计划解冻记录原料时整体核对。
	downgradeRawIDs := map[int64]bool{}
	if len(req.RawUsages) > 0 {
		for _, us := range req.RawUsages {
			var rid int64
			var st string
			if err := tx.QueryRow(ctx,
				`SELECT id,status FROM raw_material_batches WHERE batch_no=$1`, us.RawBatchNo).Scan(&rid, &st); err == nil && st == "downgraded" {
				downgraded = true
				downgradeRawIDs[rid] = true
			}
		}
	} else {
		drows, _ := tx.Query(ctx, `
			SELECT DISTINCT rm.id FROM thaw_records t
			JOIN raw_material_batches rm ON rm.id=t.raw_batch_id
			WHERE t.plan_id=$1 AND rm.status='downgraded'`, planID)
		for drows.Next() {
			var rid int64
			_ = drows.Scan(&rid)
			downgraded = true
			downgradeRawIDs[rid] = true
		}
		drows.Close()
	}
	if downgraded {
		_ = tx.QueryRow(ctx, `
			SELECT id,COALESCE(downgrade_label,'降级品·限定向渠道销售'),COALESCE(downgrade_channels,'CH04')
			FROM thaw_locks
			WHERE raw_batch_id=ANY($1::bigint[]) AND status='downgraded'
			ORDER BY decided_at DESC LIMIT 1`,
			mapKeys(downgradeRawIDs)).Scan(&lockID, &dgLabel, &dgChannels)
		if lockID == 0 {
			// 兼容手工标记的降级原料：仍给出默认标签与渠道
			_ = tx.QueryRow(ctx, `
				SELECT id,COALESCE(downgrade_label,'降级品·解冻超时复测降级'),COALESCE(downgrade_channels,'CH04')
				FROM thaw_locks WHERE plan_id=$1 ORDER BY decided_at DESC LIMIT 1`, planID).
				Scan(&lockID, &dgLabel, &dgChannels)
			if lockID == 0 {
				dgLabel, dgChannels = "降级品·解冻超时复测降级", "CH04"
			}
		}
	}

	var finID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO finished_batches(batch_no,plan_id,product_name,packaging_spec,qty_kg,cold_storage_location,
		  is_downgraded,downgrade_label,downgrade_channels,downgrade_note,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		req.BatchNo, planID, productName, req.PackagingSpec, req.QtyKg, req.ColdStorageLoc,
		downgraded, nullable(dgLabel), nullable(dgChannels),
		map[bool]string{true: "使用解冻超时复测降级原料，降级使用", false: ""}[downgraded],
		u.ID).Scan(&finID); err != nil {
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

	// 降级成品的成本核算单独标记（标准成本 40 元/kg，降级按 60%）
	costMarkNo := ""
	if downgraded {
		var firstRaw int64
		ids := mapKeys(downgradeRawIDs)
		if len(ids) > 0 {
			firstRaw = ids[0]
		}
		no, err := insertCostMark(ctx, tx, lockID, firstRaw, finID, req.QtyKg,
			dgLabel, dgChannels, "解冻超时复测降级成品成本单独核算")
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		costMarkNo = no
	}

	if _, err := tx.Exec(ctx, `UPDATE production_plans SET status='finished' WHERE id=$1`, planID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"finished_id": finID, "batch_no": req.BatchNo,
		"sample_no": sampleNo, "sample_result": result,
		"is_downgraded": downgraded, "downgrade_label": dgLabel,
		"downgrade_channels": dgChannels, "cost_mark": costMarkNo != "",
	})
}
