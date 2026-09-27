package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// 降级成本核算口径（演示固定参数）：
// 标准原料成本 40 元/kg；降级品按 60% 计价（24 元/kg），差额 16 元/kg 单独记为降级损失。
const (
	standardRawCostPerKg = 40.0
	downgradeCostFactor  = 0.6
)

// ---------- 解冻超时锁批 ----------

// listThawLocks 锁批列表：含原料/计划/事件、复测结论、半成品隔离与返工情况
func (s *Server) listThawLocks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT tl.id,tl.lock_no,rm.batch_no AS raw_batch_no,rm.species,
		       p.plan_no,tl.duration_minutes,tl.allowed_minutes,
		       tl.status,tl.retest_result,tl.disposition,
		       tl.retest_center_temp_c,tl.retest_sensory,tl.retest_by,tl.retest_at::text,
		       tl.downgrade_label,tl.downgrade_channels,
		       tl.locked_at::text,tl.decided_at::text,e.event_no,
		       (SELECT count(*) FROM semi_finished_quarantines q WHERE q.thaw_lock_id=tl.id) AS semi_count,
		       (SELECT COALESCE(sum(qty_kg),0) FROM semi_finished_quarantines q WHERE q.thaw_lock_id=tl.id) AS semi_kg
		FROM thaw_locks tl
		JOIN raw_material_batches rm ON rm.id=tl.raw_batch_id
		LEFT JOIN production_plans p ON p.id=tl.plan_id
		LEFT JOIN quality_events e ON e.id=tl.event_id
		ORDER BY tl.locked_at DESC`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}

// getThawLock 锁批档案：复测记录、处置结论、半成品隔离、降级成本标记
func (s *Server) getThawLock(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	ctx := r.Context()
	headRows, err := s.pool.Query(ctx, `
		SELECT tl.id,tl.lock_no,rm.batch_no AS raw_batch_no,rm.species,rm.qty_kg,
		       p.plan_no,p.product_name,tl.duration_minutes,tl.allowed_minutes,
		       tl.status,tl.retest_result,tl.disposition,
		       tl.retest_center_temp_c,tl.retest_sensory,tl.retest_by,tl.retest_at::text,
		       tl.downgrade_label,tl.downgrade_channels,tl.decide_note,
		       tl.locked_at::text,tl.decided_by,tl.decided_at::text,e.event_no
		FROM thaw_locks tl
		JOIN raw_material_batches rm ON rm.id=tl.raw_batch_id
		LEFT JOIN production_plans p ON p.id=tl.plan_id
		LEFT JOIN quality_events e ON e.id=tl.event_id
		WHERE tl.id=$1`, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	head := scanMaps(headRows)
	headRows.Close()
	if len(head) == 0 {
		writeErr(w, 404, "锁批记录不存在")
		return
	}
	qm := func(sql string, args ...any) []map[string]any {
		rows, qerr := s.pool.Query(ctx, sql, args...)
		if qerr != nil {
			return []map[string]any{}
		}
		defer rows.Close()
		return scanMaps(rows)
	}
	semi := qm(`
		SELECT q.id,q.quarantine_no,q.stage,q.qty_kg,q.location,q.test_result,
		       q.test_note,q.rework_allowed,q.status,q.decided_by,q.decided_at::text,
		       fb.batch_no AS reworked_batch_no
		FROM semi_finished_quarantines q
		LEFT JOIN finished_batches fb ON fb.id=q.reworked_finished_batch_id
		WHERE q.thaw_lock_id=$1 ORDER BY q.created_at`, id)
	costs := qm(`
		SELECT m.mark_no,fb.batch_no AS finished_batch_no,m.label_mark,m.channel_restriction,
		       m.unit_cost,m.standard_cost,m.loss_amount,m.cost_note,m.marked_at::text
		FROM batch_cost_marks m LEFT JOIN finished_batches fb ON fb.id=m.finished_batch_id
		WHERE m.thaw_lock_id=$1 ORDER BY m.marked_at`, id)
	writeJSON(w, 200, map[string]any{"lock": head[0], "semi_quarantines": semi, "cost_marks": costs})
}

type retestReq struct {
	CenterTempC float64 `json:"center_temp_c"` // 复测中心温度
	Sensory     string  `json:"sensory"`       // 感官状态：色泽/气味/弹性
	Result      string  `json:"result"`        // qualified / degrade / unqualified
	Note        string  `json:"note"`
}

// addRetest 品控复测中心温度与感官状态
func (s *Server) addRetest(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req retestReq
	if err := decode(r, &req); err != nil ||
		(req.Result != "qualified" && req.Result != "degrade" && req.Result != "unqualified") {
		writeErr(w, 400, "result 必须为 qualified（合格放行）/degrade（降级使用）/unqualified（不合格销毁）")
		return
	}
	u := currentUser(r)
	tag, err := s.pool.Exec(r.Context(), `
		UPDATE thaw_locks
		SET retest_center_temp_c=$2,retest_sensory=$3,retest_result=$4,retest_by=$5,retest_at=now()
		WHERE id=$1 AND status='pending'`,
		id, req.CenterTempC, req.Sensory, req.Result, u.Username)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 409, "锁批不存在或已处置（仅待复测状态可录入复测）")
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "retest_result": req.Result,
		"hint": "复测已录入，请继续提交处置结论（放行/降级/销毁）",
	})
}

type disposeLockReq struct {
	Disposition       string `json:"disposition"`        // normal_release / downgrade / destroy
	DowngradeLabel    string `json:"downgrade_label"`    // 降级成品标签
	DowngradeChannels string `json:"downgrade_channels"` // 降级品限定渠道 code，逗号分隔，如 CH04
	Note              string `json:"note"`
}

// disposeLock 锁批处置：
//   - normal_release 复测合格 → 原料回在库、计划解除暂停恢复加工；
//   - downgrade 降级使用 → 原料标记 downgraded、计划恢复，后续成品标签/渠道/成本单独标记；
//   - destroy 不合格 → 原料销毁，计划保持锁定不得加工。
func (s *Server) disposeLock(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req disposeLockReq
	if err := decode(r, &req); err != nil ||
		(req.Disposition != "normal_release" && req.Disposition != "downgrade" && req.Disposition != "destroy") {
		writeErr(w, 400, "disposition 必须为 normal_release/downgrade/destroy")
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

	var rawID, planID, eventID int64
	var status, retest *string
	if err := tx.QueryRow(ctx, `
		SELECT raw_batch_id,COALESCE(plan_id,0),COALESCE(event_id,0),status,retest_result
		FROM thaw_locks WHERE id=$1 FOR UPDATE`, id).
		Scan(&rawID, &planID, &eventID, &status, &retest); err != nil {
		writeErr(w, 404, "锁批记录不存在")
		return
	}
	if status == nil || *status != "pending" {
		writeErr(w, 409, "该锁批已处置或状态异常")
		return
	}
	if retest == nil {
		writeErr(w, 409, "品控尚未复测（中心温度+感官状态），不能处置")
		return
	}
	// 处置结论必须与复测结果匹配，防止降级/销毁被随意选择
	want := map[string]string{"normal_release": "qualified", "downgrade": "degrade", "destroy": "unqualified"}
	if *retest != want[req.Disposition] {
		labels := map[string]string{"qualified": "复测合格", "degrade": "复测建议降级", "unqualified": "复测不合格"}
		writeErr(w, 409, fmt.Sprintf(
			"处置与复测结论不一致：当前复测为「%s」，不能选择该处置（合格→放行，建议降级→降级使用，不合格→销毁）",
			labels[*retest]))
		return
	}

	label := req.DowngradeLabel
	channels := req.DowngradeChannels
	lockStatus := req.Disposition
	switch req.Disposition {
	case "normal_release":
		lockStatus = "released"
		_, _ = tx.Exec(ctx,
			`UPDATE raw_material_batches SET status='in_storage' WHERE id=$1 AND status='locked'`, rawID)
		if planID > 0 {
			_, _ = tx.Exec(ctx,
				`UPDATE production_plans SET status='thawing',locked_raw_batch_id=NULL
				 WHERE id=$1 AND status='locked'`, planID)
		}
	case "downgrade":
		if label == "" {
			label = "降级品·限定向渠道销售"
		}
		if channels == "" {
			channels = "CH04" // 默认仅餐饮经销
		}
		lockStatus = "downgraded"
		_, _ = tx.Exec(ctx, `UPDATE raw_material_batches SET status='downgraded' WHERE id=$1`, rawID)
		if planID > 0 {
			_, _ = tx.Exec(ctx,
				`UPDATE production_plans SET status='thawing',locked_raw_batch_id=NULL
				 WHERE id=$1 AND status='locked'`, planID)
		}
	case "destroy":
		lockStatus = "destroyed"
		_, _ = tx.Exec(ctx, `UPDATE raw_material_batches SET status='destroyed' WHERE id=$1`, rawID)
		// 计划保持 locked：排程持续避开，加工/完工均会被拦截
	}

	if _, err := tx.Exec(ctx, `
		UPDATE thaw_locks SET status=$2,disposition=$3,downgrade_label=$4,downgrade_channels=$5,
		  decided_by=$6,decided_at=now(),decide_note=$7
		WHERE id=$1`,
		id, lockStatus, req.Disposition, label, channels, u.Username, req.Note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if eventID > 0 {
		_, _ = tx.Exec(ctx, `UPDATE quality_events SET status='closed' WHERE id=$1`, eventID)
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "status": lockStatus, "disposition": req.Disposition,
		"downgrade_label": label, "downgrade_channels": channels,
	})
}

// ---------- 锁批期间已进入半成品：隔离 / 检测 / 返工 ----------

type semiReq struct {
	Stage    string  `json:"stage"`
	QtyKg    float64 `json:"qty_kg"`
	Location string  `json:"location"`
	Note     string  `json:"note"`
}

func (s *Server) createSemiQuarantine(w http.ResponseWriter, r *http.Request) {
	lockID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req semiReq
	if err := decode(r, &req); err != nil || req.Location == "" || req.QtyKg <= 0 {
		writeErr(w, 400, "缺少 location 或 qty_kg 非法")
		return
	}
	u := currentUser(r)
	no := docNo("SQ")
	var id int64
	err := s.pool.QueryRow(r.Context(), `
		INSERT INTO semi_finished_quarantines
		  (quarantine_no,thaw_lock_id,raw_batch_id,plan_id,stage,qty_kg,location,test_note)
		SELECT $1,$2,raw_batch_id,plan_id,$3,$4,$5,$6 FROM thaw_locks WHERE id=$2
		RETURNING id`,
		no, lockID, req.Stage, req.QtyKg, req.Location, req.Note).Scan(&id)
	if err != nil {
		writeErr(w, 400, "半成品隔离登记失败："+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"id": id, "quarantine_no": no, "location": req.Location,
		"registered_by": u.Username, "await_test": true,
	})
}

type semiResultReq struct {
	TestResult    string `json:"test_result"` // qualified / unqualified
	ReworkAllowed bool   `json:"rework_allowed"`
	Note          string `json:"note"`
}

func (s *Server) semiResult(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req semiResultReq
	if err := decode(r, &req); err != nil ||
		(req.TestResult != "qualified" && req.TestResult != "unqualified") {
		writeErr(w, 400, "test_result 必须为 qualified/unqualified")
		return
	}
	u := currentUser(r)
	// 不合格的半成品不允许返工
	allowed := req.ReworkAllowed && req.TestResult == "qualified"
	tag, err := s.pool.Exec(r.Context(), `
		UPDATE semi_finished_quarantines
		SET test_result=$2,test_note=COALESCE(NULLIF($3,''),test_note),
		    rework_allowed=$4,decided_by=$5,decided_at=now()
		WHERE id=$1 AND status='isolated'`,
		id, req.TestResult, req.Note, allowed, u.Username)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, 409, "隔离记录不存在或已处置（仅隔离中可录入检测结果）")
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "test_result": req.TestResult, "rework_allowed": allowed,
	})
}

type reworkReq struct {
	BatchNo        string  `json:"batch_no"`
	QtyKg          float64 `json:"qty_kg"`
	ColdStorageLoc string  `json:"cold_storage_location"`
	PackagingSpec  string  `json:"packaging_spec"`
	Inspector      string  `json:"inspector"`
	SampleResult   string  `json:"sample_result"` // qualified/unqualified/留空 pending
}

// reworkSemi 半成品返工：检测合格且允许返工才能执行；
// 返工产出新成品批次入库，并重新生成一条 rework 留样记录。
func (s *Server) reworkSemi(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req reworkReq
	if err := decode(r, &req); err != nil || req.BatchNo == "" || req.QtyKg <= 0 {
		writeErr(w, 400, "缺少 batch_no 或 qty_kg 非法")
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

	var lockID, rawID, planID int64
	var qStatus string
	var pending bool
	var allowed bool
	var qty float64
	if err := tx.QueryRow(ctx, `
		SELECT thaw_lock_id,COALESCE(raw_batch_id,0),COALESCE(plan_id,0),qty_kg,
		       status,(test_result IS NULL),rework_allowed
		FROM semi_finished_quarantines WHERE id=$1 FOR UPDATE`,
		id).Scan(&lockID, &rawID, &planID, &qty, &qStatus, &pending, &allowed); err != nil {
		writeErr(w, 404, "隔离记录不存在")
		return
	}
	if qStatus != "isolated" {
		writeErr(w, 409, "该隔离半成品已处置："+qStatus)
		return
	}
	if pending {
		writeErr(w, 409, "尚未录入检测结果，不能返工")
		return
	}
	if !allowed {
		writeErr(w, 409, "检测结论不允许返工（不合格半成品只能销毁）")
		return
	}

	var productName string
	if planID > 0 {
		_ = tx.QueryRow(ctx, `SELECT product_name FROM production_plans WHERE id=$1`, planID).Scan(&productName)
	}
	if productName == "" {
		productName = "返工成品"
	}
	// 继承锁批降级标记：问题原料返工后的成品仍按降级品管理
	var downgraded bool
	var dgLabel, dgChannels *string
	_ = tx.QueryRow(ctx, `
		SELECT tl.status='downgraded',tl.downgrade_label,tl.downgrade_channels
		FROM thaw_locks tl WHERE tl.id=$1`, lockID).Scan(&downgraded, &dgLabel, &dgChannels)

	var finID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO finished_batches
		  (batch_no,plan_id,product_name,packaging_spec,qty_kg,cold_storage_location,
		   is_downgraded,downgrade_label,downgrade_channels,downgrade_note,created_by)
		VALUES($1,NULLIF($2,0),$3,$4,$5,$6,$7,$8,$9,'半成品返工产出',$10) RETURNING id`,
		req.BatchNo, planID, productName+"(返工)", req.PackagingSpec, req.QtyKg,
		req.ColdStorageLoc, downgraded, dgLabel, dgChannels, u.ID).Scan(&finID); err != nil {
		writeErr(w, 400, "返工成品批次创建失败："+err.Error())
		return
	}
	if rawID > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO batch_links(finished_batch_id,raw_batch_id,qty_kg) VALUES($1,$2,$3)
			ON CONFLICT DO NOTHING`, finID, rawID, req.QtyKg); err != nil {
			writeErr(w, 400, "原料关联失败："+err.Error())
			return
		}
	}
	if req.ColdStorageLoc != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
			VALUES($1,$2,$3,'available')
			ON CONFLICT (finished_batch_id,location,status)
			DO UPDATE SET qty_kg=inventory.qty_kg+EXCLUDED.qty_kg,updated_at=now()`,
			finID, req.ColdStorageLoc, req.QtyKg); err != nil {
			writeErr(w, 400, "返工成品入库失败："+err.Error())
			return
		}
	}

	// 返工成品重新生成留样记录（sample_kind=rework）
	sampleNo := docNo("SP")
	result := "pending"
	if req.SampleResult == "qualified" || req.SampleResult == "unqualified" {
		result = req.SampleResult
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO samples(sample_no,finished_batch_id,sample_kind,result,tested_at,lab_note,inspector)
		VALUES($1,$2,'rework',$3,CASE WHEN $3='pending' THEN NULL ELSE now() END,$4,$5)`,
		sampleNo, finID, result,
		fmt.Sprintf("返工成品重新留样（隔离半成品返工，原锁批 %d）", lockID), req.Inspector); err != nil {
		writeErr(w, 400, "返工留样失败："+err.Error())
		return
	}

	if downgraded {
		if _, err := insertCostMark(ctx, tx, lockID, rawID, finID, req.QtyKg,
			*dgLabel, *dgChannels, "半成品返工降级成本核算"); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE semi_finished_quarantines SET status='reworked',reworked_finished_batch_id=$2
		WHERE id=$1`, id, finID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"finished_batch_no": req.BatchNo, "sample_no": sampleNo, "sample_result": result,
		"is_downgraded": downgraded,
	})
}

// destroySemi 检测不合格且不允许返工的半成品 → 销毁
func (s *Server) destroySemi(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	type nReq struct{ Note string }
	var req nReq
	_ = decode(r, &req)
	note := req.Note
	u := currentUser(r)
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	var qStatus string
	var pending bool
	if err := tx.QueryRow(ctx, `
		SELECT status,(test_result IS NULL) FROM semi_finished_quarantines WHERE id=$1 FOR UPDATE`,
		id).Scan(&qStatus, &pending); err != nil {
		writeErr(w, 404, "隔离记录不存在")
		return
	}
	if qStatus != "isolated" {
		writeErr(w, 409, "该隔离半成品已处置："+qStatus)
		return
	}
	if pending {
		writeErr(w, 409, "尚未录入检测结果，不能销毁")
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE semi_finished_quarantines SET status='destroyed',decided_by=$2,decided_at=now(),
		  test_note=COALESCE(NULLIF($3,''),test_note) WHERE id=$1`,
		id, u.Username, note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": "destroyed"})
}

// insertCostMark 在事务内为降级成品写入成本单独标记：
// 标准成本 40 元/kg，降级按 60% 计价，差额计入降级损失。
func insertCostMark(ctx context.Context, tx pgx.Tx, lockID, rawID, finID int64, qty float64,
	label, channels, note string) (string, error) {
	standard := round2(qty * standardRawCostPerKg)
	unit := round2(standardRawCostPerKg * downgradeCostFactor)
	downgradedCost := round2(qty * standardRawCostPerKg * downgradeCostFactor)
	loss := round2(standard - downgradedCost)
	markNo := docNo("CM")
	_, err := tx.Exec(ctx, `
		INSERT INTO batch_cost_marks
		  (mark_no,thaw_lock_id,raw_batch_id,finished_batch_id,label_mark,channel_restriction,
		   unit_cost,standard_cost,loss_amount,cost_note,marked_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		markNo, nullZeroInt(lockID), nullZeroInt(rawID), finID, label, channels,
		unit, standard, loss, note, "system")
	if err != nil {
		return "", err
	}
	return markNo, nil
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func mapKeys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullZeroInt(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

// channelAllowed 校验降级成品的发货渠道是否在允许名单内。
func channelAllowed(restriction, channelCode string) bool {
	if strings.TrimSpace(restriction) == "" {
		return true
	}
	for _, c := range strings.Split(restriction, ",") {
		if strings.TrimSpace(c) == channelCode {
			return true
		}
	}
	return false
}

// listCostMarks 降级成本核算标记台账：成品标签 / 限定渠道 / 标准成本 / 降级成本 / 损失
func (s *Server) listCostMarks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT m.id,m.mark_no,tl.lock_no,rm.batch_no AS raw_batch_no,
		       fb.batch_no AS finished_batch_no,m.label_mark,m.channel_restriction,
		       m.unit_cost,m.standard_cost,m.loss_amount,m.cost_note,m.marked_at::text
		FROM batch_cost_marks m
		LEFT JOIN thaw_locks tl ON tl.id=m.thaw_lock_id
		LEFT JOIN raw_material_batches rm ON rm.id=m.raw_batch_id
		LEFT JOIN finished_batches fb ON fb.id=m.finished_batch_id
		ORDER BY m.marked_at DESC`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}
