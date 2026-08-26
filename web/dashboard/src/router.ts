import { createRouter, createWebHistory } from "vue-router";
import OverviewView from "./views/OverviewView.vue";
import ServicesView from "./views/ServicesView.vue";
import NodesView from "./views/NodesView.vue";
import SettingsView from "./views/SettingsView.vue";
import HelpView from "./views/HelpView.vue";

export default createRouter({
  history: createWebHistory("/dashboard/"),
  routes: [
    { path: "/", redirect: "/overview" },
    { path: "/overview", component: OverviewView, meta: { title: "概览" } },
    { path: "/services", component: ServicesView, meta: { title: "内网服务" } },
    { path: "/nodes", component: NodesView, meta: { title: "节点" } },
    { path: "/settings", component: SettingsView, meta: { title: "设置" } },
    { path: "/help", component: HelpView, meta: { title: "帮助" } },
  ],
  scrollBehavior(to) {
    if (to.hash) return { el: to.hash, behavior: "smooth" };
    return { top: 0 };
  },
});
