-- 海鲜预制菜工厂 批次解冻加工与召回服务 schema

CREATE TABLE IF NOT EXISTS users (
  id BIGSERIAL PRIMARY KEY,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  full_name TEXT,
  role TEXT NOT NULL CHECK (role IN ('admin','manager','quality','warehouse','production','finance')),
  active BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS suppliers (
  id BIGSERIAL PRIMARY KEY,
  code TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  contact TEXT,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended'))
);

CREATE TABLE IF NOT EXISTS raw_material_batches (
  id BIGSERIAL PRIMARY KEY,
  batch_no TEXT UNIQUE NOT NULL,
  supplier_id BIGINT REFERENCES suppliers(id),
  species TEXT NOT NULL CHECK (species IN ('虾仁','扇贝','鱿鱼','鱼柳')),
  catch_or_production_date DATE NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  inbound_temp_c NUMERIC(5,2) NOT NULL,
  quarantine_cert TEXT NOT NULL,
  box_no TEXT NOT NULL,
  location TEXT NOT NULL,
  qty_kg NUMERIC(10,2) NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'in_storage'
    CHECK (status IN ('in_storage','locked','consumed','recalled','destroyed','downgraded')),
  created_by BIGINT REFERENCES users(id)
);
CREATE INDEX IF NOT EXISTS idx_raw_supplier ON raw_material_batches(supplier_id);

CREATE TABLE IF NOT EXISTS temp_logs (
  id BIGSERIAL PRIMARY KEY,
  raw_batch_id BIGINT NOT NULL REFERENCES raw_material_batches(id) ON DELETE CASCADE,
  temp_c NUMERIC(5,2) NOT NULL,
  recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  operator TEXT,
  note TEXT,
  alarm BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS idx_templog_raw ON temp_logs(raw_batch_id);

CREATE TABLE IF NOT EXISTS production_lines (
  id BIGSERIAL PRIMARY KEY,
  code TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  active BOOLEAN NOT NULL DEFAULT true
);

CREATE TABLE IF NOT EXISTS seasoning_batches (
  id BIGSERIAL PRIMARY KEY,
  batch_no TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  supplier TEXT,
  expiry_date DATE
);

CREATE TABLE IF NOT EXISTS production_plans (
  id BIGSERIAL PRIMARY KEY,
  plan_no TEXT UNIQUE NOT NULL,
  product_name TEXT NOT NULL,
  line_id BIGINT REFERENCES production_lines(id),
  target_qty_kg NUMERIC(10,2),
  packaging_spec TEXT,                 -- 渠道定制包装要求
  required_seasoning_batch TEXT,       -- 计划指定调味料批次
  status TEXT NOT NULL DEFAULT 'planned'
    CHECK (status IN ('planned','thawing','processing','finished','cancelled','locked')),
  locked_raw_batch_id BIGINT REFERENCES raw_material_batches(id), -- 解冻超时锁批：暂停加工线的问题原料
  planned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by BIGINT REFERENCES users(id)
);

CREATE TABLE IF NOT EXISTS thaw_records (
  id BIGSERIAL PRIMARY KEY,
  plan_id BIGINT NOT NULL REFERENCES production_plans(id) ON DELETE CASCADE,
  raw_batch_id BIGINT REFERENCES raw_material_batches(id),
  start_at TIMESTAMPTZ NOT NULL,
  end_at TIMESTAMPTZ NOT NULL,
  center_temp_c NUMERIC(5,2),
  water_batch_no TEXT,
  operator TEXT,
  duration_minutes INT GENERATED ALWAYS AS
    (CAST(EXTRACT(EPOCH FROM (end_at - start_at)) / 60 AS INT)) STORED,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_thaw_plan ON thaw_records(plan_id);

CREATE TABLE IF NOT EXISTS process_records (
  id BIGSERIAL PRIMARY KEY,
  plan_id BIGINT NOT NULL REFERENCES production_plans(id) ON DELETE CASCADE,
  stage TEXT NOT NULL CHECK (stage IN ('wash','trim','season','weigh','package','metalcheck','coldstorage')),
  operator TEXT,
  value_num NUMERIC(12,2),
  value_text TEXT,
  ok BOOLEAN NOT NULL DEFAULT true,
  note TEXT,
  recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_process_plan ON process_records(plan_id);

CREATE TABLE IF NOT EXISTS finished_batches (
  id BIGSERIAL PRIMARY KEY,
  batch_no TEXT UNIQUE NOT NULL,
  plan_id BIGINT REFERENCES production_plans(id),
  product_name TEXT NOT NULL,
  packaging_spec TEXT,
  qty_kg NUMERIC(10,2) NOT NULL DEFAULT 0,
  cold_storage_location TEXT,
  status TEXT NOT NULL DEFAULT 'in_stock'
    CHECK (status IN ('in_stock','released','recalled','quarantined','destroyed','reworked')),
  is_downgraded BOOLEAN NOT NULL DEFAULT false,   -- 解冻超时复测降级：降级使用标记
  downgrade_label TEXT,                           -- 成品标签降级字样（如“降级品·限餐饮经销”）
  downgrade_channels TEXT,                        -- 允许销售渠道，逗号分隔（空=不限制，仅作降级默认渠道建议）
  downgrade_note TEXT,
  produced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by BIGINT REFERENCES users(id)
);
CREATE INDEX IF NOT EXISTS idx_finished_plan ON finished_batches(plan_id);

CREATE TABLE IF NOT EXISTS batch_links (
  id BIGSERIAL PRIMARY KEY,
  finished_batch_id BIGINT NOT NULL REFERENCES finished_batches(id) ON DELETE CASCADE,
  raw_batch_id BIGINT NOT NULL REFERENCES raw_material_batches(id),
  qty_kg NUMERIC(10,2) NOT NULL DEFAULT 0,
  UNIQUE (finished_batch_id, raw_batch_id)
);
CREATE INDEX IF NOT EXISTS idx_links_raw ON batch_links(raw_batch_id);
CREATE INDEX IF NOT EXISTS idx_links_fin ON batch_links(finished_batch_id);

CREATE TABLE IF NOT EXISTS samples (
  id BIGSERIAL PRIMARY KEY,
  sample_no TEXT UNIQUE NOT NULL,
  finished_batch_id BIGINT NOT NULL REFERENCES finished_batches(id) ON DELETE CASCADE,
  sample_kind TEXT NOT NULL DEFAULT 'normal' CHECK (sample_kind IN ('normal','rework')), -- 返工成品留样单独标记
  source_batch_id BIGINT REFERENCES finished_batches(id), -- 返工来源成品批次
  taken_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  result TEXT NOT NULL DEFAULT 'pending' CHECK (result IN ('pending','qualified','unqualified')),
  tested_at TIMESTAMPTZ,
  lab_note TEXT,
  inspector TEXT
);
CREATE INDEX IF NOT EXISTS idx_samples_fin ON samples(finished_batch_id);

CREATE TABLE IF NOT EXISTS channels (
  id BIGSERIAL PRIMARY KEY,
  code TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  ctype TEXT NOT NULL CHECK (ctype IN ('商超','电商','直播','经销'))
);

CREATE TABLE IF NOT EXISTS shipments (
  id BIGSERIAL PRIMARY KEY,
  ship_no TEXT UNIQUE NOT NULL,
  channel_id BIGINT REFERENCES channels(id),
  shipped_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  status TEXT NOT NULL DEFAULT 'shipped' CHECK (status IN ('shipped','recalled','closed'))
);
CREATE INDEX IF NOT EXISTS idx_ship_channel ON shipments(channel_id);

CREATE TABLE IF NOT EXISTS shipment_items (
  id BIGSERIAL PRIMARY KEY,
  shipment_id BIGINT NOT NULL REFERENCES shipments(id) ON DELETE CASCADE,
  finished_batch_id BIGINT REFERENCES finished_batches(id),
  boxes INT NOT NULL DEFAULT 0,
  qty_kg NUMERIC(10,2) NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_si_ship ON shipment_items(shipment_id);
CREATE INDEX IF NOT EXISTS idx_si_fin ON shipment_items(finished_batch_id);

CREATE TABLE IF NOT EXISTS orders (
  id BIGSERIAL PRIMARY KEY,
  order_no TEXT UNIQUE NOT NULL,
  shipment_id BIGINT REFERENCES shipments(id),
  finished_batch_id BIGINT REFERENCES finished_batches(id),
  channel_id BIGINT REFERENCES channels(id),
  box_no TEXT NOT NULL,
  qty NUMERIC(10,2) NOT NULL DEFAULT 0,
  customer TEXT,
  status TEXT NOT NULL DEFAULT 'sold' CHECK (status IN ('sold','recalled','returned','compensated')),
  sold_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_orders_fin ON orders(finished_batch_id);
CREATE INDEX IF NOT EXISTS idx_orders_ship ON orders(shipment_id);

CREATE TABLE IF NOT EXISTS channel_feedback (
  id BIGSERIAL PRIMARY KEY,
  channel_id BIGINT REFERENCES channels(id),
  order_id BIGINT REFERENCES orders(id),
  finished_batch_id BIGINT REFERENCES finished_batches(id),
  ftype TEXT NOT NULL,
  content TEXT,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  handled BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS quality_events (
  id BIGSERIAL PRIMARY KEY,
  event_no TEXT UNIQUE NOT NULL,
  etype TEXT NOT NULL CHECK (etype IN
    ('temp_exceed','thaw_timeout','seasoning_mismatch','metal_alarm','package_leak',
     'channel_odor','live_return','regulatory_recall')),
  severity TEXT NOT NULL DEFAULT 'major' CHECK (severity IN ('minor','major','critical')),
  raw_batch_id BIGINT REFERENCES raw_material_batches(id),
  finished_batch_id BIGINT REFERENCES finished_batches(id),
  order_id BIGINT REFERENCES orders(id),
  description TEXT,
  status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','recalled','closed')),
  created_by BIGINT REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_events_raw ON quality_events(raw_batch_id);
CREATE INDEX IF NOT EXISTS idx_events_fin ON quality_events(finished_batch_id);

CREATE TABLE IF NOT EXISTS recalls (
  id BIGSERIAL PRIMARY KEY,
  recall_no TEXT UNIQUE NOT NULL,
  event_id BIGINT REFERENCES quality_events(id),
  level TEXT NOT NULL DEFAULT 'class2' CHECK (level IN ('class1','class2','class3')),
  reason TEXT,
  status TEXT NOT NULL DEFAULT 'executing' CHECK (status IN ('executing','closed')),
  initiated_by BIGINT REFERENCES users(id),
  initiated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS recall_actions (
  id BIGSERIAL PRIMARY KEY,
  recall_id BIGINT NOT NULL REFERENCES recalls(id) ON DELETE CASCADE,
  atype TEXT NOT NULL CHECK (atype IN ('stop_sale','return','compensation','supplier_claim')),
  doc_no TEXT NOT NULL,
  title TEXT,
  content TEXT,
  amount NUMERIC(12,2) NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'issued',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_actions_recall ON recall_actions(recall_id);

CREATE TABLE IF NOT EXISTS return_goods (
  id BIGSERIAL PRIMARY KEY,
  return_no TEXT UNIQUE NOT NULL,
  recall_id BIGINT REFERENCES recalls(id),
  finished_batch_id BIGINT REFERENCES finished_batches(id),
  box_no TEXT,
  qty_kg NUMERIC(10,2) NOT NULL DEFAULT 0,
  arrived_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  disposition TEXT CHECK (disposition IN ('reinspect','rework','destroy')),
  decided_by TEXT,
  decided_at TIMESTAMPTZ,
  note TEXT
);
CREATE INDEX IF NOT EXISTS idx_return_fin ON return_goods(finished_batch_id);

CREATE TABLE IF NOT EXISTS inventory (
  id BIGSERIAL PRIMARY KEY,
  finished_batch_id BIGINT NOT NULL REFERENCES finished_batches(id),
  location TEXT NOT NULL,
  qty_kg NUMERIC(10,2) NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'available'
    CHECK (status IN ('available','quarantined','reworked','destroyed')),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (finished_batch_id, location, status)
);
CREATE INDEX IF NOT EXISTS idx_inv_fin ON inventory(finished_batch_id);

CREATE TABLE IF NOT EXISTS stocktakes (
  id BIGSERIAL PRIMARY KEY,
  stocktake_no TEXT UNIQUE NOT NULL,
  location TEXT,
  counted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  operator TEXT,
  note TEXT
);

CREATE TABLE IF NOT EXISTS stocktake_items (
  id BIGSERIAL PRIMARY KEY,
  stocktake_id BIGINT NOT NULL REFERENCES stocktakes(id) ON DELETE CASCADE,
  finished_batch_id BIGINT REFERENCES finished_batches(id),
  system_qty NUMERIC(10,2),
  counted_qty NUMERIC(10,2),
  diff_qty NUMERIC(10,2)
);

CREATE TABLE IF NOT EXISTS supplier_deductions (
  id BIGSERIAL PRIMARY KEY,
  deduction_no TEXT UNIQUE NOT NULL,
  supplier_id BIGINT REFERENCES suppliers(id),
  recall_id BIGINT REFERENCES recalls(id),
  raw_batch_id BIGINT REFERENCES raw_material_batches(id),
  amount NUMERIC(12,2) NOT NULL,
  reason TEXT,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','confirmed','deducted')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS customer_compensations (
  id BIGSERIAL PRIMARY KEY,
  comp_no TEXT UNIQUE NOT NULL,
  order_id BIGINT REFERENCES orders(id),
  amount NUMERIC(12,2) NOT NULL,
  reason TEXT,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','paid')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS regulatory_samplings (
  id BIGSERIAL PRIMARY KEY,
  sampling_no TEXT UNIQUE NOT NULL,
  regulator TEXT NOT NULL,
  target_type TEXT NOT NULL CHECK (target_type IN ('raw','finished')),
  raw_batch_id BIGINT REFERENCES raw_material_batches(id),
  finished_batch_id BIGINT REFERENCES finished_batches(id),
  sampled_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  result TEXT NOT NULL DEFAULT 'pending' CHECK (result IN ('pending','qualified','unqualified')),
  note TEXT
);

-- ========== 解冻超时锁批（thaw lock）==========
-- 解冻间发现某批鱿鱼超过允许时间（240 分钟）：暂停该批进入加工线，品控复测中心温度与感官状态。
CREATE TABLE IF NOT EXISTS thaw_locks (
  id BIGSERIAL PRIMARY KEY,
  lock_no TEXT UNIQUE NOT NULL,
  event_id BIGINT REFERENCES quality_events(id),      -- 关联自动生成的 thaw_timeout 事件
  raw_batch_id BIGINT NOT NULL REFERENCES raw_material_batches(id),
  plan_id BIGINT REFERENCES production_plans(id),
  thaw_record_id BIGINT REFERENCES thaw_records(id),
  duration_minutes INT NOT NULL,
  allowed_minutes INT NOT NULL DEFAULT 240,
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending','released','downgraded','destroyed')), -- 待复测/放行/降级使用/销毁
  -- 品控复测：中心温度 + 感官状态
  retest_center_temp_c NUMERIC(5,2),
  retest_sensory TEXT,                                 -- 感官状态（色泽/气味/弹性）
  retest_result TEXT CHECK (retest_result IN ('qualified','degrade','unqualified')),
  retest_by TEXT,
  retest_at TIMESTAMPTZ,
  -- 处置结论
  disposition TEXT CHECK (disposition IN ('normal_release','downgrade','destroy')),
  downgrade_label TEXT,                                   -- 降级成品标签字样
  downgrade_channels TEXT,                                -- 降级品允许销售渠道（渠道 code 逗号分隔）
  decided_by TEXT,
  decided_at TIMESTAMPTZ,
  decide_note TEXT,
  locked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by BIGINT REFERENCES users(id)
);
CREATE INDEX IF NOT EXISTS idx_thawlock_raw ON thaw_locks(raw_batch_id);
CREATE INDEX IF NOT EXISTS idx_thawlock_status ON thaw_locks(status);

-- 锁批期间原料已进入半成品：隔离库位、检测结果、是否允许返工
CREATE TABLE IF NOT EXISTS semi_finished_quarantines (
  id BIGSERIAL PRIMARY KEY,
  quarantine_no TEXT UNIQUE NOT NULL,
  thaw_lock_id BIGINT NOT NULL REFERENCES thaw_locks(id) ON DELETE CASCADE,
  raw_batch_id BIGINT REFERENCES raw_material_batches(id),
  plan_id BIGINT REFERENCES production_plans(id),
  stage TEXT,                                           -- 隔离时所处工序（wash/trim/season...）
  qty_kg NUMERIC(10,2) NOT NULL DEFAULT 0,
  location TEXT NOT NULL,                               -- 隔离库位
  test_result TEXT CHECK (test_result IN ('pending','qualified','unqualified')),
  test_note TEXT,
  rework_allowed BOOLEAN NOT NULL DEFAULT false,        -- 是否允许返工
  decided_by TEXT,
  decided_at TIMESTAMPTZ,
  status TEXT NOT NULL DEFAULT 'isolated'
    CHECK (status IN ('isolated','reworked','destroyed','released')),
  reworked_finished_batch_id BIGINT REFERENCES finished_batches(id), -- 返工产出的成品批次
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_semiq_lock ON semi_finished_quarantines(thaw_lock_id);

-- 降级使用：成品标签 / 销售渠道 / 成本核算单独标记
CREATE TABLE IF NOT EXISTS batch_cost_marks (
  id BIGSERIAL PRIMARY KEY,
  mark_no TEXT UNIQUE NOT NULL,
  thaw_lock_id BIGINT REFERENCES thaw_locks(id),
  raw_batch_id BIGINT REFERENCES raw_material_batches(id),
  finished_batch_id BIGINT REFERENCES finished_batches(id),
  label_mark TEXT NOT NULL DEFAULT '降级品',             -- 成品标签标记
  channel_restriction TEXT,                             -- 限定销售渠道（渠道 code 逗号分隔）
  unit_cost NUMERIC(12,2) NOT NULL DEFAULT 0,           -- 降级后单位成本
  standard_cost NUMERIC(12,2) NOT NULL DEFAULT 0,       -- 标准单位成本
  loss_amount NUMERIC(12,2) NOT NULL DEFAULT 0,         -- 降级损失金额
  cost_note TEXT,
  marked_by TEXT,
  marked_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_costmark_fin ON batch_cost_marks(finished_batch_id);

-- ========== 幂等迁移（兼容旧库：补列、收紧/放宽约束）==========
ALTER TABLE production_plans ADD COLUMN IF NOT EXISTS locked_raw_batch_id BIGINT REFERENCES raw_material_batches(id);

ALTER TABLE finished_batches ADD COLUMN IF NOT EXISTS is_downgraded BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE finished_batches ADD COLUMN IF NOT EXISTS downgrade_label TEXT;
ALTER TABLE finished_batches ADD COLUMN IF NOT EXISTS downgrade_channels TEXT;
ALTER TABLE finished_batches ADD COLUMN IF NOT EXISTS downgrade_note TEXT;

ALTER TABLE samples ADD COLUMN IF NOT EXISTS sample_kind TEXT NOT NULL DEFAULT 'normal';
ALTER TABLE samples ADD COLUMN IF NOT EXISTS source_batch_id BIGINT REFERENCES finished_batches(id);

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname='raw_material_batches_status_check') THEN
    ALTER TABLE raw_material_batches DROP CONSTRAINT raw_material_batches_status_check;
  END IF;
  ALTER TABLE raw_material_batches ADD CONSTRAINT raw_material_batches_status_check
    CHECK (status IN ('in_storage','locked','consumed','recalled','destroyed','downgraded'));

  IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname='production_plans_status_check') THEN
    ALTER TABLE production_plans DROP CONSTRAINT production_plans_status_check;
  END IF;
  ALTER TABLE production_plans ADD CONSTRAINT production_plans_status_check
    CHECK (status IN ('planned','thawing','processing','finished','cancelled','locked'));
END $$;
