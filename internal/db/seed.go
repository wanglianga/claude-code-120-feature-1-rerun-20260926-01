package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Seed 幂等写入演示数据（全部 ON CONFLICT DO NOTHING）。
func Seed(ctx context.Context, p *pgxpool.Pool) error {
	users := []struct{ user, pass, name, role string }{
		{"admin", "admin123", "系统管理员", "admin"},
		{"manager", "manager123", "工厂经理", "manager"},
		{"quality", "quality123", "品控主管", "quality"},
		{"warehouse", "ware123", "冷库仓管", "warehouse"},
		{"production", "prod123", "产线班长", "production"},
		{"finance", "finance123", "财务专员", "finance"},
	}
	for _, u := range users {
		h, err := bcrypt.GenerateFromPassword([]byte(u.pass), bcrypt.MinCost)
		if err != nil {
			return err
		}
		if _, err := p.Exec(ctx, `
			INSERT INTO users(username,password_hash,full_name,role)
			VALUES($1,$2,$3,$4) ON CONFLICT (username) DO NOTHING`,
			u.user, string(h), u.name, u.role); err != nil {
			return err
		}
	}

	stmts := []string{
		`INSERT INTO suppliers(code,name,contact) VALUES
		 ('S001','荣成远洋水产有限公司','王经理 13800000001'),
		 ('S002','大连獐子岛渔业合作社','李经理 13800000002'),
		 ('S003','舟山鱿鱼捕捞合作社','张经理 13800000003'),
		 ('S004','北海鱼柳加工厂','赵经理 13800000004')
		 ON CONFLICT (code) DO NOTHING`,

		`INSERT INTO production_lines(code,name) VALUES
		 ('L1','一号解冻加工线'),('L2','二号解冻加工线')
		 ON CONFLICT (code) DO NOTHING`,

		`INSERT INTO seasoning_batches(batch_no,name,supplier,expiry_date) VALUES
		 ('T001','蒜香腌料','味之源食品','2027-01-01'),
		 ('T002','麻辣腌料','味之源食品','2027-03-01')
		 ON CONFLICT (batch_no) DO NOTHING`,

		// 4 个原料批次：RM03 入库温度 -15.1 超过冻品 ≤-18℃ 限值
		`INSERT INTO raw_material_batches
		 (batch_no,supplier_id,species,catch_or_production_date,inbound_temp_c,quarantine_cert,box_no,location,qty_kg,status,created_by)
		 SELECT 'RM20260901',id,'虾仁','2026-08-28',-18.20,'QY2026-0001','BX-R-001','冷库A-01',500,'in_storage',1 FROM suppliers WHERE code='S001'
		 ON CONFLICT (batch_no) DO NOTHING`,
		`INSERT INTO raw_material_batches
		 (batch_no,supplier_id,species,catch_or_production_date,inbound_temp_c,quarantine_cert,box_no,location,qty_kg,status,created_by)
		 SELECT 'RM20260902',id,'扇贝','2026-08-29',-17.80,'QY2026-0002','BX-S-001','冷库A-02',300,'in_storage',1 FROM suppliers WHERE code='S002'
		 ON CONFLICT (batch_no) DO NOTHING`,
		`INSERT INTO raw_material_batches
		 (batch_no,supplier_id,species,catch_or_production_date,inbound_temp_c,quarantine_cert,box_no,location,qty_kg,status,created_by)
		 SELECT 'RM20260903',id,'鱿鱼','2026-08-30',-15.10,'QY2026-0003','BX-U-001','冷库A-03',400,'locked',1 FROM suppliers WHERE code='S003'
		 ON CONFLICT (batch_no) DO NOTHING`,
		`INSERT INTO raw_material_batches
		 (batch_no,supplier_id,species,catch_or_production_date,inbound_temp_c,quarantine_cert,box_no,location,qty_kg,status,created_by)
		 SELECT 'RM20260904',id,'鱼柳','2026-08-31',-18.50,'QY2026-0004','BX-F-001','冷库B-01',600,'in_storage',1 FROM suppliers WHERE code='S004'
		 ON CONFLICT (batch_no) DO NOTHING`,

		`INSERT INTO temp_logs(raw_batch_id,temp_c,operator,note,alarm)
		 SELECT id,-15.10,'仓管孙七','入库测温高于 -18℃ 限值，锁定待判',true
		 FROM raw_material_batches WHERE batch_no='RM20260903'
		   AND NOT EXISTS(SELECT 1 FROM temp_logs tl WHERE tl.raw_batch_id=raw_material_batches.id AND tl.alarm)`,

		// 生产计划（含渠道定制包装要求）
		`INSERT INTO production_plans(plan_no,product_name,line_id,target_qty_kg,packaging_spec,required_seasoning_batch,status,created_by)
		 SELECT 'PP20260910-01','蒜香虾仁',id,240,'直播间定制 250g 彩包','T001','finished',1 FROM production_lines WHERE code='L1'
		 ON CONFLICT (plan_no) DO NOTHING`,
		`INSERT INTO production_plans(plan_no,product_name,line_id,target_qty_kg,packaging_spec,required_seasoning_batch,status,created_by)
		 SELECT 'PP20260910-02','麻辣鱿鱼花',id,180,'商超定制 500g 简包','T002','finished',1 FROM production_lines WHERE code='L2'
		 ON CONFLICT (plan_no) DO NOTHING`,

		// 解冻记录
		`INSERT INTO thaw_records(plan_id,raw_batch_id,start_at,end_at,center_temp_c,water_batch_no,operator)
		 SELECT p.id,r.id,'2026-09-10 06:00+08','2026-09-10 09:00+08',2.10,'W-260910-01','刘解冻'
		 FROM production_plans p, raw_material_batches r
		 WHERE p.plan_no='PP20260910-01' AND r.batch_no='RM20260901'
		   AND NOT EXISTS(SELECT 1 FROM thaw_records t WHERE t.plan_id=p.id)`,
		`INSERT INTO thaw_records(plan_id,raw_batch_id,start_at,end_at,center_temp_c,water_batch_no,operator)
		 SELECT p.id,r.id,'2026-09-10 06:30+08','2026-09-10 11:20+08',3.40,'W-260910-02','刘解冻'
		 FROM production_plans p, raw_material_batches r
		 WHERE p.plan_no='PP20260910-02' AND r.batch_no='RM20260903'
		   AND NOT EXISTS(SELECT 1 FROM thaw_records t WHERE t.plan_id=p.id)`,

		// 加工七步记录（wash/trim/season/weigh/package/metalcheck/coldstorage）
		`INSERT INTO process_records(plan_id,stage,operator,value_num,value_text,ok,note)
		 SELECT p.id,s.st,'周加工',s.vn,s.vt,s.ok,s.nt FROM production_plans p
		 CROSS JOIN (VALUES
		   ('wash','清水三道漂洗',NULL::numeric,'电导率 820us/cm',true,''),
		   ('trim','去线分切',NULL,'规格 3g/只',true,''),
		   ('season','按 T001 投料',NULL,'T001',true,'蒜香腌料 12kg'),
		   ('weigh','称重',0.25,'250g/盒',true,''),
		   ('package','直播彩包充氮',NULL,'250g 彩包',true,''),
		   ('metalcheck','金检 Fe1.5/NonFe2.0/Sus2.5',NULL,'通过',true,''),
		   ('coldstorage','入成品冷库',-18.0,'冷库C-01',true,'')
		 ) AS s(st,nt,vn,vt,ok,ign)
		 WHERE p.plan_no='PP20260910-01'
		   AND NOT EXISTS(SELECT 1 FROM process_records pr WHERE pr.plan_id=p.id)`,
		`INSERT INTO process_records(plan_id,stage,operator,value_num,value_text,ok,note)
		 SELECT p.id,s.st,'马加工',s.vn,s.vt,s.ok,s.nt FROM production_plans p
		 CROSS JOIN (VALUES
		   ('wash','清水三道漂洗',NULL::numeric,'电导率 910us/cm',true,''),
		   ('trim','切花刀',NULL,'4cm 花刀',true,''),
		   ('season','按 T002 投料',NULL,'T002',true,'麻辣腌料 9kg'),
		   ('weigh','称重',0.50,'500g/盒',true,''),
		   ('package','商超简包',NULL,'500g 简包',true,''),
		   ('metalcheck','金检 Fe1.5/NonFe2.0/Sus2.5',NULL,'通过',true,''),
		   ('coldstorage','入成品冷库',-18.0,'冷库C-02',true,'')
		 ) AS s(st,nt,vn,vt,ok,ign)
		 WHERE p.plan_no='PP20260910-02'
		   AND NOT EXISTS(SELECT 1 FROM process_records pr WHERE pr.plan_id=p.id)`,

		// 成品批次
		`INSERT INTO finished_batches(batch_no,plan_id,product_name,packaging_spec,qty_kg,cold_storage_location,created_by)
		 SELECT 'FG20260912-01',id,'蒜香虾仁','250g 直播彩包',240,'冷库C-01',1 FROM production_plans WHERE plan_no='PP20260910-01'
		 ON CONFLICT (batch_no) DO NOTHING`,
		`INSERT INTO finished_batches(batch_no,plan_id,product_name,packaging_spec,qty_kg,cold_storage_location,created_by)
		 SELECT 'FG20260912-02',id,'麻辣鱿鱼花','500g 商超简包',180,'冷库C-02',1 FROM production_plans WHERE plan_no='PP20260910-02'
		 ON CONFLICT (batch_no) DO NOTHING`,

		// 原料-成品关联
		`INSERT INTO batch_links(finished_batch_id,raw_batch_id,qty_kg)
		 SELECT f.id,r.id,240 FROM finished_batches f, raw_material_batches r
		 WHERE f.batch_no='FG20260912-01' AND r.batch_no='RM20260901'
		 ON CONFLICT DO NOTHING`,
		`INSERT INTO batch_links(finished_batch_id,raw_batch_id,qty_kg)
		 SELECT f.id,r.id,180 FROM finished_batches f, raw_material_batches r
		 WHERE f.batch_no='FG20260912-02' AND r.batch_no='RM20260903'
		 ON CONFLICT DO NOTHING`,

		// 留样
		`INSERT INTO samples(sample_no,finished_batch_id,result,tested_at,lab_note,inspector)
		 SELECT 'SP-001',id,'qualified','2026-09-12 16:00+08','微生物/感官均合格','品控钱九'
		 FROM finished_batches WHERE batch_no='FG20260912-01'
		 ON CONFLICT (sample_no) DO NOTHING`,
		`INSERT INTO samples(sample_no,finished_batch_id,result,tested_at,lab_note,inspector)
		 SELECT 'SP-002',id,'qualified','2026-09-12 16:10+08','初检合格','品控钱九'
		 FROM finished_batches WHERE batch_no='FG20260912-02'
		 ON CONFLICT (sample_no) DO NOTHING`,

		// 渠道
		`INSERT INTO channels(code,name,ctype) VALUES
		 ('CH01','盒马鲜生(华东)','商超'),('CH02','天猫旗舰店','电商'),
		 ('CH03','抖音海鲜直播间','直播'),('CH04','华东餐饮经销商','经销')
		 ON CONFLICT (code) DO NOTHING`,

		// 发货
		`INSERT INTO shipments(ship_no,channel_id,status)
		 SELECT 'SH20260913-01',id,'shipped' FROM channels WHERE code='CH03'
		 ON CONFLICT (ship_no) DO NOTHING`,
		`INSERT INTO shipments(ship_no,channel_id,status)
		 SELECT 'SH20260913-02',id,'shipped' FROM channels WHERE code='CH01'
		 ON CONFLICT (ship_no) DO NOTHING`,
		`INSERT INTO shipment_items(shipment_id,finished_batch_id,boxes,qty_kg)
		 SELECT s.id,f.id,200,50 FROM shipments s, finished_batches f
		 WHERE s.ship_no='SH20260913-01' AND f.batch_no='FG20260912-01'
		   AND NOT EXISTS(SELECT 1 FROM shipment_items si WHERE si.shipment_id=s.id)`,
		`INSERT INTO shipment_items(shipment_id,finished_batch_id,boxes,qty_kg)
		 SELECT s.id,f.id,160,80 FROM shipments s, finished_batches f
		 WHERE s.ship_no='SH20260913-01' AND f.batch_no='FG20260912-02'
		   AND NOT EXISTS(SELECT 1 FROM shipment_items si WHERE si.shipment_id=s.id AND si.finished_batch_id=f.id)`,
		`INSERT INTO shipment_items(shipment_id,finished_batch_id,boxes,qty_kg)
		 SELECT s.id,f.id,300,75 FROM shipments s, finished_batches f
		 WHERE s.ship_no='SH20260913-02' AND f.batch_no='FG20260912-01'
		   AND NOT EXISTS(SELECT 1 FROM shipment_items si WHERE si.shipment_id=s.id)`,

		// 已售订单（精确到箱）
		`INSERT INTO orders(order_no,shipment_id,finished_batch_id,channel_id,box_no,qty,customer,status)
		 SELECT 'OD20260914-001',s.id,f.id,c.id,'BX-LIVE-0001',2,'直播观众周**','sold'
		 FROM shipments s, finished_batches f, channels c
		 WHERE s.ship_no='SH20260913-01' AND f.batch_no='FG20260912-02' AND c.code='CH03'
		 ON CONFLICT (order_no) DO NOTHING`,
		`INSERT INTO orders(order_no,shipment_id,finished_batch_id,channel_id,box_no,qty,customer,status)
		 SELECT 'OD20260914-002',s.id,f.id,c.id,'BX-LIVE-0002',1,'直播观众吴**','sold'
		 FROM shipments s, finished_batches f, channels c
		 WHERE s.ship_no='SH20260913-01' AND f.batch_no='FG20260912-02' AND c.code='CH03'
		 ON CONFLICT (order_no) DO NOTHING`,
		`INSERT INTO orders(order_no,shipment_id,finished_batch_id,channel_id,box_no,qty,customer,status)
		 SELECT 'OD20260914-003',s.id,f.id,c.id,'BX-LIVE-0003',3,'直播观众郑**','sold'
		 FROM shipments s, finished_batches f, channels c
		 WHERE s.ship_no='SH20260913-01' AND f.batch_no='FG20260912-01' AND c.code='CH03'
		 ON CONFLICT (order_no) DO NOTHING`,
		`INSERT INTO orders(order_no,shipment_id,finished_batch_id,channel_id,box_no,qty,customer,status)
		 SELECT 'OD20260915-010',s.id,f.id,c.id,'BX-HM-0010',10,'盒马张江店','sold'
		 FROM shipments s, finished_batches f, channels c
		 WHERE s.ship_no='SH20260913-02' AND f.batch_no='FG20260912-01' AND c.code='CH01'
		 ON CONFLICT (order_no) DO NOTHING`,

		// 渠道反馈：直播间反馈鱿鱼花异味
		`INSERT INTO channel_feedback(channel_id,order_id,finished_batch_id,ftype,content,handled)
		 SELECT c.id,o.id,f.id,'odor','顾客反馈解冻后有氨味，直播间集中差评',false
		 FROM channels c, orders o, finished_batches f
		 WHERE c.code='CH03' AND o.order_no='OD20260914-001' AND f.batch_no='FG20260912-02'
		   AND NOT EXISTS(SELECT 1 FROM channel_feedback WHERE ftype='odor')`,

		// 冷库库存（剩余库存）
		`INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
		 SELECT id,'冷库C-01',115,'available' FROM finished_batches WHERE batch_no='FG20260912-01'
		 ON CONFLICT DO NOTHING`,
		`INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
		 SELECT id,'冷库C-02',100,'available' FROM finished_batches WHERE batch_no='FG20260912-02'
		 ON CONFLICT DO NOTHING`,

		// 一条未结案的品控事件：鱿鱼原料入库温度超标（尚未召回，供演示处置）
		`INSERT INTO quality_events(event_no,etype,severity,raw_batch_id,description,status,created_by)
		 SELECT 'EV20260903-01','temp_exceed','major',id,'RM20260903 入库温度 -15.1℃，高于冻品限值 -18℃','open',3
		 FROM raw_material_batches WHERE batch_no='RM20260903'
		 ON CONFLICT (event_no) DO NOTHING`,

		// ===== 解冻超时锁批演示场景（RM20260920：解冻 305 分钟超时，已进入半成品）=====

		// 新鱿鱼原料：入库温度正常，解冻环节超时
		`INSERT INTO raw_material_batches
		 (batch_no,supplier_id,species,catch_or_production_date,inbound_temp_c,quarantine_cert,box_no,location,qty_kg,status,created_by)
		 SELECT 'RM20260920',id,'鱿鱼','2026-09-18',-18.30,'QY2026-0020','BX-U-020','冷库A-04',200,'locked',1 FROM suppliers WHERE code='S003'
		 ON CONFLICT (batch_no) DO NOTHING`,

		// 在制计划：已上 L2 产线，解冻后完成清洗/切配/调味，停在包装前（半成品阶段）
		`INSERT INTO production_plans(plan_no,product_name,line_id,target_qty_kg,packaging_spec,required_seasoning_batch,status,created_by)
		 SELECT 'PP20260925-01','麻辣鱿鱼花',id,150,'商超定制 500g 简包（降级后限餐饮渠道）','T002','processing',1 FROM production_lines WHERE code='L2'
		 ON CONFLICT (plan_no) DO NOTHING`,

		// 解冻记录：06:00→11:05 共 305 分钟，超过 240 分钟限值
		`INSERT INTO thaw_records(plan_id,raw_batch_id,start_at,end_at,center_temp_c,water_batch_no,operator)
		 SELECT p.id,r.id,'2026-09-25 06:00+08','2026-09-25 11:05+08',4.50,'W-260925-01','刘解冻'
		 FROM production_plans p, raw_material_batches r
		 WHERE p.plan_no='PP20260925-01' AND r.batch_no='RM20260920'
		   AND NOT EXISTS(SELECT 1 FROM thaw_records t WHERE t.plan_id=p.id AND t.raw_batch_id=r.id)`,

		// 已完成三道工序：清洗、切配、调味（半成品已形成，尚未包装/金检/完工）
		`INSERT INTO process_records(plan_id,stage,operator,value_num,value_text,ok,note)
		 SELECT p.id,s.st,'马加工',s.vn,s.vt,s.ok,s.nt FROM production_plans p
		 CROSS JOIN (VALUES
		   ('wash','清水三道漂洗',NULL::numeric,'电导率 880us/cm',true,''),
		   ('trim','切花刀',NULL,'4cm 花刀',true,''),
		   ('season','按 T002 投料',NULL,'T002',true,'麻辣腌料 7.5kg，调味后待包装')
		 ) AS s(st,nt,vn,vt,ok,ign)
		 WHERE p.plan_no='PP20260925-01'
		   AND NOT EXISTS(SELECT 1 FROM process_records pr WHERE pr.plan_id=p.id)`,

		// 半成品批次（调味后、包装前），150kg 暂存在待包装区
		`INSERT INTO finished_batches(batch_no,plan_id,product_name,packaging_spec,qty_kg,cold_storage_location,status,created_by)
		 SELECT 'FG20260925-S1',id,'麻辣鱿鱼花(半成品·调味后待包装)','待包装',150,'冷库C-03-待包装暂存','in_stock',1
		 FROM production_plans WHERE plan_no='PP20260925-01'
		 ON CONFLICT (batch_no) DO NOTHING`,

		`INSERT INTO batch_links(finished_batch_id,raw_batch_id,qty_kg)
		 SELECT f.id,r.id,150 FROM finished_batches f, raw_material_batches r
		 WHERE f.batch_no='FG20260925-S1' AND r.batch_no='RM20260920'
		 ON CONFLICT DO NOTHING`,

		`INSERT INTO inventory(finished_batch_id,location,qty_kg,status)
		 SELECT id,'冷库C-03-待包装暂存',150,'available' FROM finished_batches WHERE batch_no='FG20260925-S1'
		 ON CONFLICT DO NOTHING`,

		// 解冻超时品控事件（open，等待品控复测闭环）
		`INSERT INTO quality_events(event_no,etype,severity,raw_batch_id,description,status,created_by)
		 SELECT 'EV20260920-01','thaw_timeout','major',id,
		        '解冻用时 305 分钟，超过限值 240 分钟（计划 PP20260925-01，用水批次 W-260925-01），已自动锁批；该批已进入半成品（FG20260925-S1）',
		        'open',3
		 FROM raw_material_batches WHERE batch_no='RM20260920'
		 ON CONFLICT (event_no) DO NOTHING`,

		// 活动锁批单：原料/计划/解冻/事件/半成品全关联，等待复测 → 隔离 → 返工
		`INSERT INTO thaw_locks(lock_no,raw_batch_id,plan_id,thaw_record_id,event_id,duration_minutes,status,locked_by,semi_finished_id)
		 SELECT 'LK20260920-01',r.id,p.id,t.id,e.id,305,'locked',3,f.id
		 FROM raw_material_batches r, production_plans p, thaw_records t,
		      quality_events e, finished_batches f
		 WHERE r.batch_no='RM20260920' AND p.plan_no='PP20260925-01'
		   AND t.plan_id=p.id AND t.raw_batch_id=r.id
		   AND e.event_no='EV20260920-01' AND f.batch_no='FG20260925-S1'
		 ON CONFLICT (lock_no) DO NOTHING`,
	}
	for _, s := range stmts {
		if _, err := p.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}
