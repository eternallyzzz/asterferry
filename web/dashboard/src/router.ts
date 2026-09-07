import { createRouter, createWebHistory } from "vue-router";
import { useSession } from "./session";

declare module "vue-router" {
  interface RouteMeta {
    title?: string;
    requiresAdmin?: boolean;
  }
}

const router = createRouter({
  history: createWebHistory("/dashboard/"),
  routes: [
    { path: "/", redirect: "/overview" },
    { path: "/overview", name: "overview", component: () => import("./pages/OverviewPage.vue"), meta: { title: "概览" } },
    { path: "/nodes", name: "nodes", component: () => import("./pages/NodesPage.vue"), meta: { title: "节点" } },
    { path: "/services", name: "services", component: () => import("./pages/ServicesPage.vue"), meta: { title: "服务" } },
    { path: "/assignments", name: "assignments", component: () => import("./pages/AssignmentsPage.vue"), meta: { title: "调度" } },
    { path: "/activity", name: "activity", component: () => import("./pages/ActivityPage.vue"), meta: { title: "活动" } },
    { path: "/admin", name: "admin", component: () => import("./pages/AdminPage.vue"), meta: { title: "管理", requiresAdmin: true } },
    { path: "/:pathMatch(.*)*", redirect: "/overview" },
  ],
});

// This is the route-side half of the admin route/action guard; App's login
// gate handles unauthenticated users.
router.beforeEach((to) => {
  if (to.meta.requiresAdmin && useSession().controllerUser.value?.role !== "admin") {
    return { name: "overview" };
  }
  return true;
});

router.afterEach((to) => {
  document.title = to.meta.title ? `${to.meta.title} · AsterFerry` : "AsterFerry";
});

export default router;
