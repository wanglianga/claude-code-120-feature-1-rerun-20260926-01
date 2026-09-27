package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ---------- 解冻超时锁批 ----------

// channelNames 把 "CH04,CH02" 形式的渠道编码串翻译成 "CH04 华东餐饮经销商"。
func (s *Server) channelNames(ctx context.Context, codes string) string {
	codes = strings.TrimSpace(codes)
	if codes == "" {
		return ""
	}
	parts := strings.Split(codes, ",")
	names := make([]string, 0, len(parts))
	for _, c := range parts {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		var name string
		if err := s.pool.QueryRow(ctx, `SELECT code||' '||name FROM channels WHERE code=$1`, c).Scan(&name); err == nil {
			names = append(names, name)
		} else {
			names = append(names, c)
		}
	}
	return strings.Join(names, "；")
}

func (s *Server) listThawLocks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT l.id,l.lock_no,rm.batch_no AS raw_batch_no,rm.species,rm.qty_kg,
		       p.plan_no,l.duration_minutes,l.status,l.locked_at::text,
		       l.retest_center_temp_c,l.retest_sensory,l.retest_result,
		       l.retest_by,l.retest_at::text,l.retest_note,
		       l.label_text,l.channel_restriction,l.allow_rework,
		       f.batch_no AS semi_finished_batch_no,l.quarantine_location,
		       l.quarantine_result,l.quarantined_by,l.quarantined_at::text,l.quarantine_note,
		       e.event_no,l.closed_at::text
		FROM thaw_locks l
		JOIN raw_material_batches rm ON rm.id=l.raw_batch_id
		LEFT JOIN production_plans p ON p.id=l.plan_id
		LEFT JOIN finished_batches f ON f.id=l.semi_finished_id
		LEFT JOIN quality_events e ON e.id=l.event_id
		ORDER BY l.locked_at DESC`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := scanMaps(rows)
	rows.Close()
	for _, m := range out {
		if codes, _ := m["channel_restriction"].(string); codes != "" {
			m["channel_names"] = s.channelNames(r.Context(), codes)
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) getThawLock(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	ctx := r.Context()
	headRows, err := s.pool.Query(ctx, `
		SELECT l.*, rm.batch_no AS raw_batch_no, rm.species, p.plan_no, e.event_no,
		       f.batch_no AS semi_finished_batch_no
		FROM thaw_locks l
		JOIN raw_material_batches rm ON rm.id=l.raw_batch_id
		LEFT JOIN production_plans p ON p.id=l.plan_id
		LEFT JOIN quality_events e ON e.id=l.event_id
		LEFT JOIN finished_batches f ON f.id=l.semi_finished_id
		WHERE l.id=$1`, id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	head := scanMaps(headRows)
	headRows.Close()
	if len(head) == 0 {
		writeErr(w, 404, "锁批单不存在")
		return
	}
	l := head[0]
	if codes, _ := l["channel_restriction"].(string); codes != "" {
		l["channel_names"] = s.channelNames(ctx, codes)
	}

	qm := func(sql string, args ...any) []map[string]any {
		rows, qerr := s.pool.Query(ctx, sql, args...)
		if qerr != nil {
			return []map[string]any{}
		}
		defer rows.Close()
		return scanMaps(rows)
	}
	rawID := l["raw_batch_id"]
	costs := qm(`
		SELECT cost_no,qty_kg,normal_price,downgrade_price,price_loss,rework_cost,
		       total_cost,cost_center,note,created_at::text
		FROM downgrade_costs WHERE thaw_lock_id=$1 ORDER BY created_at`, id)
	related := qm(`
		SELECT f.batch_no,f.product_name,f.status,f.label_mark,f.label_text,
		       f.channel_restriction,f.rework_of_finished_id,fb.batch_no AS rework_of_batch_no
		FROM batch_links b
		JOIN finished_batches f ON f.id=b.finished_batch_id
		LEFT JOIN finished_batches fb ON fb.id=f.rework_of_finished_id
		WHERE b.raw_batch_id=$1 ORDER BY f.produced_at`, rawID)
	samples := qm(`
		SELECT f.batch_no,s.sample_no,s.result,s.taken_at::text,s.lab_note,s.inspector
		FROM samples s JOIN finished_batches f ON f.id=s.finished_batch_id
		WHERE f.id IN (SELECT finished_batch_id FROM batch_links WHERE raw_batch_id=$1)
		      OR f.rework_of_finished_id IN (SELECT finished_batch_id FROM batch_links WHERE raw_batch_id=$1)
		ORDER BY s.taken_at`, rawID)
	writeJSON(w, 200, map[string]any{
		"lock": l, "costs": costs, "related_finished": related, "samples": samples,
	})
}

type retestReq struct {
	CenterTempC     float64 `json:"center_temp_c"`
	Sensory         string  `json:"sensory"`
	Result          string  `json:"result"` // qualified / downgrade / unqualified
	LabelText       string  `json:"label_text"`
	ChannelRestrict string  `json:"channel_restriction"` // 允许的降级渠道编码，逗号分隔，如 CH04
	AllowRework     bool    `json:"allow_rework"`
	Note            string  `json:"note"`
	Inspector       string  `json:"inspector"`
}

// retestLock 品控复测中心温度与感官状态，给出 放行 / 降级使用 / 拒收 结论。
func (s *Server) retestLock(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req retestReq
	if err := decode(r, &req); err != nil ||
		(req.Result != "qualified" && req.Result != "downgrade" && req.Result != "unqualified") {
		writeErr(w, 400, "result 必须为 qualified/downgrade/unqualified")
		return
	}
	if req.Result != "unqualified" && req.Sensory == "" {
		writeErr(w, 400, "请填写感官状态（色泽/气味/弹性）")
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

	var rawID, lockStatus int64
	var status string
	var eventID *int64
	if err := tx.QueryRow(ctx,
		`SELECT raw_batch_id,status,COALESCE(event_id,0) FROM thaw_locks WHERE id=$1 FOR UPDATE`,
		id).Scan(&rawID, &status, &lockStatus); err != nil {
		writeErr(w, 404, "锁批单不存在")
		return
	}
	if status != "locked" {
		writeErr(w, 409, "锁批单已处置："+status)
		return
	}
	if lockStatus != 0 {
		eventID = &lockStatus
	}

	label := req.LabelText
	channels := strings.TrimSpace(req.ChannelRestrict)
	if req.Result == "downgrade" {
		if channels == "" {
			channels = "CH04" // 默认仅限餐饮经销（熟食加工渠道）
		}
		if label == "" {
			label = "【解冻超时降级品】中心温度/感官降级，仅限熟食加工渠道，禁止生食及直播/商超零售"
		}
	}
	newStatus := map[string]string{
		"qualified": "released", "downgrade": "downgraded", "unqualified": "rejected",
	}[req.Result]

	if _, err := tx.Exec(ctx, `
		UPDATE thaw_locks
		SET retest_center_temp_c=$2,retest_sensory=$3,retest_result=$4,retest_note=$5,
		    retest_by=$6,retest_at=now(),
		    status=$7,label_text=$8,channel_restriction=$9,
		    allow_rework=CASE WHEN $7='downgraded' THEN $10 ELSE false END,
		    closed_at=now()
		WHERE id=$1`,
		id, req.CenterTempC, req.Sensory, req.Result, req.Note,
		firstNonEmpty(req.Inspector, u.Username), newStatus, label, channels, req.AllowRework); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	switch req.Result {
	case "qualified":
		// 复测合格：无其他活动锁时恢复在库，允许重新排产
		_, _ = tx.Exec(ctx, `
			UPDATE raw_material_batches SET status='in_storage'
			WHERE id=$1 AND status='locked'
			  AND NOT EXISTS(SELECT 1 FROM thaw_locks tl
			                 WHERE tl.raw_batch_id=$1 AND tl.id<>$2 AND tl.status='locked')`,
			rawID, id)
	case "downgrade":
		// 降级使用：原料可受控领用，成品标签/渠道/成本单独标记（完工时落标）
		_, _ = tx.Exec(ctx, `
			UPDATE raw_material_batches SET status='in_storage'
			WHERE id=$1 AND status='locked'`, rawID)
	case "unqualified":
		_, _ = tx.Exec(ctx, `UPDATE raw_material_batches SET status='destroyed' WHERE id=$1`, rawID)
	}
	// 复测有结论后闭环对应的解冻超时事件
	if eventID != nil {
		_, _ = tx.Exec(ctx, `UPDATE quality_events SET status='closed' WHERE id=$1 AND status='open'`, *eventID)
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "status": newStatus, "label_text": label,
		"channel_restriction": channels,
		"channel_names":       s.channelNames(r.Context(), channels),
	})
}

type isolateReq struct {
	SemiFinishedBatchNo string `json:"semi_finished_batch_no"`
	QuarantineLocation  string `json:"quarantine_location"`
	Result              string `json:"quarantine_result"` // pending / qualified / unqualified
	Note                string `json:"quarantine_note"`
}

// isolateSemi 锁批时原料已进入半成品：登记隔离库位与检测结果；
// 检测不合格强制不允许返工。半成品可用库存同步转隔离状态。
func (s *Server) isolateSemi(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req isolateReq
	if err := decode(r, &req); err != nil ||
		(req.Result != "pending" && req.Result != "qualified" && req.Result != "unqualified") {
		writeErr(w, 400, "quarantine_result 必须为 pending/qualified/unqualified")
		return
	}
	if req.QuarantineLocation == "" {
		writeErr(w, 400, "缺少隔离库位 quarantine_location")
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

	var status string
	var semiID *int64
	if err := tx.QueryRow(ctx,
		`SELECT status,semi_finished_id FROM thaw_locks WHERE id=$1 FOR UPDATE`, id).Scan(&status, &semiID); err != nil {
		writeErr(w, 404, "锁批单不存在")
		return
	}
	if status != "locked" && status != "downgraded" {
		writeErr(w, 409, "当前锁批状态不允许登记隔离："+status)
		return
	}

	if semiID == nil {
		if req.SemiFinishedBatchNo == "" {
			writeErr(w, 400, "该锁批尚未关联半成品，请填写 semi_finished_batch_no")
			return
		}
		var sid int64
		if err := tx.QueryRow(ctx,
			`SELECT id FROM finished_batches WHERE batch_no=$1`, req.SemiFinishedBatchNo).Scan(&sid); err != nil {
			writeErr(w, 404, "半成品/成品批次不存在："+req.SemiFinishedBatchNo)
			return
		}
		semiID = &sid
	}

	// 半成品批次转隔离；可用库存整体移入隔离库位的 quarantined 桶（先汇总插入，再删除原可用行，避免双算）
	if _, err := tx.Exec(ctx,
		`UPDATE finished_batches SET status='quarantined' WHERE id=$1 AND status<>'destroyed'`, *semiID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
		SELECT $1,$2,sum(qty_kg),'quarantined' FROM inventory
		WHERE finished_batch_id=$1 AND status='available'
		GROUP BY finished_batch_id
		HAVING sum(qty_kg) > 0
		ON CONFLICT (finished_batch_id,location,status)
		DO UPDATE SET qty_kg=inventory.qty_kg+EXCLUDED.qty_kg,updated_at=now()`,
		*semiID, req.QuarantineLocation); err != nil {
		writeErr(w, 400, "隔离库存写入失败："+err.Error())
		return
	}
	_, _ = tx.Exec(ctx,
		`DELETE FROM inventory WHERE finished_batch_id=$1 AND status='available'`, *semiID)

	if _, err := tx.Exec(ctx, `
		UPDATE thaw_locks SET semi_finished_id=$2,quarantine_location=$3,quarantine_result=$4,
		    quarantine_note=$5,quarantined_by=$6,quarantined_at=now(),
		    allow_rework=CASE WHEN $4='unqualified' THEN false ELSE allow_rework END
		WHERE id=$1`,
		id, *semiID, req.QuarantineLocation, req.Result, req.Note, u.Username); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "semi_finished_id": *semiID,
		"quarantine_location": req.QuarantineLocation,
		"quarantine_result":   req.Result,
		"allow_rework":        req.Result != "unqualified",
	})
}

type reworkReq struct {
	NewBatchNo          string  `json:"new_batch_no"`
	QtyKg               float64 `json:"qty_kg"`
	ColdStorageLocation string  `json:"cold_storage_location"`
	PackagingSpec       string  `json:"packaging_spec"`
	SampleResult        string  `json:"sample_result"` // qualified/unqualified/空 pending
	Inspector           string  `json:"inspector"`
	ReworkCostPerKg     float64 `json:"rework_cost_per_kg"`
	Note                string  `json:"note"`
}

// reworkSemi 返工被隔离的半成品：仅当复测结论为降级、明确允许返工且隔离检测合格；
// 产出新成品批次（继承降级标签与渠道限制、溯源原批次），并重新生成留样记录、单列返工成本。
func (s *Server) reworkSemi(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req reworkReq
	if err := decode(r, &req); err != nil || req.NewBatchNo == "" || req.QtyKg <= 0 {
		writeErr(w, 400, "缺少 new_batch_no 或 qty_kg")
		return
	}
	result := "pending"
	if req.SampleResult == "qualified" || req.SampleResult == "unqualified" {
		result = req.SampleResult
	}
	u := currentUser(r)
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	var status, label, channels string
	var allowRework bool
	var semiID *int64
	var qResult *string
	if err := tx.QueryRow(ctx, `
		SELECT status,COALESCE(label_text,''),COALESCE(channel_restriction,''),
		       allow_rework,semi_finished_id,quarantine_result
		FROM thaw_locks WHERE id=$1 FOR UPDATE`,
		id).Scan(&status, &label, &channels, &allowRework, &semiID, &qResult); err != nil {
		writeErr(w, 404, "锁批单不存在")
		return
	}
	if status != "downgraded" {
		writeErr(w, 409, "仅复测结论为降级使用的锁批可执行返工，当前："+status)
		return
	}
	if !allowRework {
		writeErr(w, 409, "品控未批准返工（allow_rework=false）")
		return
	}
	if semiID == nil {
		writeErr(w, 409, "锁批未登记被隔离的半成品，无法返工")
		return
	}
	if qResult == nil || *qResult != "qualified" {
		writeErr(w, 409, "半成品隔离检测未合格，不允许返工")
		return
	}

	var productName, oldPackaging string
	if err := tx.QueryRow(ctx,
		`SELECT product_name,COALESCE(packaging_spec,'') FROM finished_batches WHERE id=$1`,
		*semiID).Scan(&productName, &oldPackaging); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	packaging := req.PackagingSpec
	if packaging == "" {
		packaging = oldPackaging
	}

	// 原半成品转返工状态；其隔离库存整体转返工桶（已被返工耗用）
	if _, err := tx.Exec(ctx, `UPDATE finished_batches SET status='reworked' WHERE id=$1`, *semiID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
		SELECT finished_batch_id,location||'-返工区',sum(qty_kg),'reworked'
		FROM inventory WHERE finished_batch_id=$1 AND status='quarantined'
		GROUP BY finished_batch_id,location
		ON CONFLICT (finished_batch_id,location,status)
		DO UPDATE SET qty_kg=inventory.qty_kg+EXCLUDED.qty_kg,updated_at=now()`, *semiID); err != nil {
		writeErr(w, 400, "返工库存写入失败："+err.Error())
		return
	}
	_, _ = tx.Exec(ctx,
		`DELETE FROM inventory WHERE finished_batch_id=$1 AND status='quarantined'`, *semiID)

	// 返工产出新成品：降级标签 + 渠道限制 + 返工溯源
	var newFinID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO finished_batches(batch_no,plan_id,product_name,packaging_spec,qty_kg,
		  cold_storage_location,status,label_mark,label_text,channel_restriction,
		  rework_of_finished_id,created_by)
		SELECT $1,plan_id,$2,$3,$4,$5,'in_stock','downgraded',$6,$7,$8,$9
		FROM finished_batches WHERE id=$8
		RETURNING id`,
		req.NewBatchNo, productName+"(返工)", packaging, req.QtyKg,
		req.ColdStorageLocation, label, channels, *semiID, u.ID).Scan(&newFinID); err != nil {
		writeErr(w, 400, "返工成品创建失败："+err.Error())
		return
	}

	// 继承原料关联，保持全链可追溯
	if _, err := tx.Exec(ctx, `
		INSERT INTO batch_links(finished_batch_id,raw_batch_id,qty_kg)
		SELECT $1,raw_batch_id,$2 FROM batch_links WHERE finished_batch_id=$3
		ON CONFLICT DO NOTHING`, newFinID, req.QtyKg, *semiID); err != nil {
		writeErr(w, 400, "原料关联继承失败："+err.Error())
		return
	}
	// 返工成品入冷库
	if req.ColdStorageLocation != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
			VALUES($1,$2,$3,'available')
			ON CONFLICT (finished_batch_id,location,status)
			DO UPDATE SET qty_kg=inventory.qty_kg+EXCLUDED.qty_kg,updated_at=now()`,
			newFinID, req.ColdStorageLocation, req.QtyKg); err != nil {
			writeErr(w, 400, "返工成品入库失败："+err.Error())
			return
		}
	}
	// 返工成品重新生成留样记录
	sampleNo := docNo("SP-RW")
	if _, err := tx.Exec(ctx, `
		INSERT INTO samples(sample_no,finished_batch_id,result,tested_at,lab_note,inspector)
		VALUES($1,$2,$3,CASE WHEN $3='pending' THEN NULL ELSE now() END,
		       CASE WHEN $3='pending' THEN '返工成品待检留样' ELSE '返工成品重新检测' END,$4)`,
		sampleNo, newFinID, result, req.Inspector); err != nil {
		writeErr(w, 400, "返工留样失败："+err.Error())
		return
	}

	// 返工费用单列成本（默认 8 元/kg）
	costPerKg := req.ReworkCostPerKg
	if costPerKg <= 0 {
		costPerKg = 8
	}
	reworkCost := costPerKg * req.QtyKg
	if _, err := tx.Exec(ctx, `
		INSERT INTO downgrade_costs(cost_no,thaw_lock_id,raw_batch_id,finished_batch_id,
		  qty_kg,normal_price,downgrade_price,price_loss,rework_cost,total_cost,cost_center,note)
		SELECT $1,$2,raw_batch_id,$3,$4,0,0,0,$5,$5,'降级返工费',$6
		FROM thaw_locks WHERE id=$2`,
		docNo("COST"), id, newFinID, req.QtyKg, reworkCost,
		fmt.Sprintf("返工成品 %s，返工费 %.2f 元/kg", req.NewBatchNo, costPerKg)); err != nil {
		writeErr(w, 400, "返工成本核算失败："+err.Error())
		return
	}
	_, _ = tx.Exec(ctx, `UPDATE thaw_locks SET closed_at=now() WHERE id=$1`, id)

	if err := tx.Commit(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"finished_id": newFinID, "batch_no": req.NewBatchNo,
		"rework_of": func() string {
			var bn string
			_ = s.pool.QueryRow(r.Context(), `SELECT batch_no FROM finished_batches WHERE id=$1`, *semiID).Scan(&bn)
			return bn
		}(),
		"sample_no": sampleNo, "sample_result": result,
		"label_mark": "downgraded", "rework_cost": reworkCost,
	})
}

// scheduleMaterials 生产排程物料视图：自动避开锁批期间（含复测中）的原料批次。
func (s *Server) scheduleMaterials(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := s.pool.Query(ctx, `
		SELECT rm.id,rm.batch_no,sp.code AS supplier_code,sp.name AS supplier_name,
		       rm.species,rm.qty_kg,rm.status,rm.location,
		       EXISTS(SELECT 1 FROM thaw_locks l
		         WHERE l.raw_batch_id=rm.id AND l.status='downgraded') AS downgraded,
		       (SELECT l.lock_no FROM thaw_locks l
		         WHERE l.raw_batch_id=rm.id AND l.status='locked'
		         ORDER BY l.locked_at DESC LIMIT 1) AS active_lock_no,
		       (SELECT l.duration_minutes FROM thaw_locks l
		         WHERE l.raw_batch_id=rm.id AND l.status='locked'
		         ORDER BY l.locked_at DESC LIMIT 1) AS lock_duration
		FROM raw_material_batches rm
		LEFT JOIN suppliers sp ON sp.id=rm.supplier_id
		ORDER BY rm.received_at DESC`)
	out := scanMaps(rows)
	rows.Close()
	available, excluded := []map[string]any{}, []map[string]any{}
	statusReason := map[string]string{
		"locked":    "原料批次锁定待判（温控超标或解冻超时），排程自动避开",
		"consumed":  "原料已耗用",
		"recalled":  "原料已召回，禁止领用",
		"destroyed": "原料已销毁",
	}
	for _, m := range out {
		locked, _ := m["active_lock_no"].(string)
		status, _ := m["status"].(string)
		switch {
		case locked != "":
			m["exclude_reason"] = "解冻超时锁批，等待品控复测，排程自动避开"
			excluded = append(excluded, m)
		case status != "in_storage":
			reason := statusReason[status]
			if reason == "" {
				reason = "原料不可用（" + status + "）"
			}
			m["exclude_reason"] = reason
			excluded = append(excluded, m)
		default:
			available = append(available, m)
		}
	}
	writeJSON(w, 200, map[string]any{
		"available": available, "excluded": excluded,
		"note": "锁批（status=locked）原料不得排产/解冻；降级批仅可在标签与渠道限制下受控使用",
	})
}

// ---------- 降级品成本核算 ----------

func (s *Server) listDowngradeCosts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT c.id,c.cost_no,l.lock_no,rm.batch_no AS raw_batch_no,
		       f.batch_no AS finished_batch_no,f.label_text,
		       c.qty_kg,c.normal_price,c.downgrade_price,c.price_loss,
		       c.rework_cost,c.total_cost,c.cost_center,c.note,c.created_at::text
		FROM downgrade_costs c
		LEFT JOIN thaw_locks l ON l.id=c.thaw_lock_id
		LEFT JOIN raw_material_batches rm ON rm.id=c.raw_batch_id
		LEFT JOIN finished_batches f ON f.id=c.finished_batch_id
		ORDER BY c.created_at DESC`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	writeJSON(w, 200, scanMaps(rows))
}
