package api

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

//go:embed all:web
var webFS embed.FS

type Server struct {
	pool *pgxpool.Pool
	rdb  *redis.Client
}

func NewServer(pool *pgxpool.Pool, rdb *redis.Client) *Server {
	return &Server{pool: pool, rdb: rdb}
}

type ctxKey string

const userCtx ctxKey = "user"

type sessionUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
	Role     string `json:"role"`
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/api", func(r chi.Router) {
		r.Post("/auth/login", s.login)
		r.Post("/auth/logout", s.logout)

		r.Group(func(r chi.Router) {
			r.Use(s.auth)
			r.Get("/auth/me", s.me)
			r.Get("/dashboard", s.dashboard)

			// 基础档案
			r.Get("/suppliers", s.listSuppliers)
			r.Get("/lines", s.listLines)
			r.Get("/channels", s.listChannels)
			r.Get("/seasonings", s.listSeasonings)

			// 采购入库 / 仓储温控
			r.Get("/raw-batches", s.listRawBatches)
			r.With(role("warehouse", "admin", "manager")).Post("/raw-batches", s.createRawBatch)
			r.Get("/raw-batches/{batchNo}/trace", s.rawTrace)
			r.With(role("warehouse", "admin", "manager")).Post("/raw-batches/{batchNo}/temp-logs", s.addTempLog)

			// 生产：计划 / 解冻 / 加工 / 完工
			r.Get("/plans", s.listPlans)
			r.With(role("production", "admin", "manager")).Post("/plans", s.createPlan)
			r.With(role("production", "admin", "manager")).Post("/plans/{id}/thaw", s.addThaw)
			r.With(role("production", "admin", "manager")).Post("/plans/{id}/process", s.addProcess)
			r.With(role("production", "admin", "manager")).Post("/plans/{id}/finish", s.finishPlan)

			// 解冻超时锁批：复测 / 处置 / 半成品隔离检测 / 返工
			r.Get("/thaw-locks", s.listThawLocks)
			r.Get("/thaw-locks/{id}", s.getThawLock)
			r.With(role("quality", "admin", "manager")).Post("/thaw-locks/{id}/retest", s.addRetest)
			r.With(role("quality", "admin", "manager")).Post("/thaw-locks/{id}/dispose", s.disposeLock)
			r.With(role("production", "warehouse", "quality", "admin", "manager")).Post("/thaw-locks/{id}/semi", s.createSemiQuarantine)
			r.With(role("quality", "admin", "manager")).Post("/semi-quarantines/{id}/result", s.semiResult)
			r.With(role("quality", "production", "admin", "manager")).Post("/semi-quarantines/{id}/rework", s.reworkSemi)
			r.With(role("quality", "warehouse", "admin", "manager")).Post("/semi-quarantines/{id}/destroy", s.destroySemi)
			r.Get("/cost-marks", s.listCostMarks)

			// 成品 / 留样
			r.Get("/finished-batches", s.listFinished)
			r.Get("/finished-batches/{batchNo}/trace", s.finishedTrace)
			r.With(role("quality", "admin", "manager")).Post("/finished-batches/{batchNo}/sample", s.addSample)

			// 渠道 / 发货 / 订单 / 反馈
			r.Get("/shipments", s.listShipments)
			r.With(role("production", "warehouse", "admin", "manager")).Post("/shipments", s.createShipment)
			r.Get("/orders", s.listOrders)
			r.With(role("production", "warehouse", "admin", "manager")).Post("/orders", s.createOrder)
			r.Get("/feedback", s.listFeedback)
			r.With(role("quality", "warehouse", "admin", "manager")).Post("/feedback", s.createFeedback)

			// 品控事件
			r.Get("/events", s.listEvents)
			r.With(role("quality", "admin", "manager")).Post("/events", s.createEvent)

			// 召回
			r.Get("/recalls", s.listRecalls)
			r.With(role("quality", "admin", "manager")).Post("/recalls", s.createRecall)
			r.Get("/recalls/{id}", s.getRecall)
			r.With(role("quality", "admin", "manager")).Post("/recalls/{id}/close", s.closeRecall)

			// 退货回厂处置
			r.Get("/returns", s.listReturns)
			r.With(role("warehouse", "admin", "manager")).Post("/returns", s.createReturn)
			r.With(role("quality", "admin", "manager")).Post("/returns/{id}/dispose", s.disposeReturn)

			// 库存 / 盘点
			r.Get("/inventory", s.listInventory)
			r.Get("/stocktakes", s.listStocktakes)
			r.With(role("warehouse", "admin", "manager")).Post("/stocktakes", s.createStocktake)

			// 供应商扣款
			r.Get("/deductions", s.listDeductions)
			r.With(role("finance", "admin", "manager")).Post("/deductions", s.createDeduction)
			r.With(role("finance", "admin", "manager")).Post("/deductions/{id}/status", s.updateDeduction)

			// 客服赔付 / 财务追偿
			r.Get("/compensations", s.listCompensations)
			r.With(role("quality", "finance", "admin", "manager")).Post("/compensations", s.createCompensation)
			r.With(role("finance", "admin", "manager")).Post("/compensations/{id}/pay", s.payCompensation)

			// 监管抽样
			r.Get("/regulatory-samplings", s.listSamplings)
			r.With(role("quality", "admin", "manager")).Post("/regulatory-samplings", s.createSampling)
			r.With(role("quality", "admin", "manager")).Post("/regulatory-samplings/{id}/result", s.samplingResult)
		})
	})

	// 静态前端（SPA）
	sub, _ := fs.Sub(webFS, "web")
	fileServer := http.FileServer(http.FS(sub))
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			if f, err := sub.Open(r.URL.Path[1:]); err == nil {
				f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		// 未命中的非 API 路径回退 index.html
		data, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
	return r
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func currentUser(r *http.Request) sessionUser {
	return r.Context().Value(userCtx).(sessionUser)
}
