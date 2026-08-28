import { createRouter, createWebHistory } from "vue-router";

// The Controller shell owns its resource tabs.  Keep a router instance for
// host applications that install the Dashboard as a plugin, but do not import
// the retired per-node management views or configuration routes.
export default createRouter({
  history: createWebHistory("/dashboard/"),
  routes: [],
});
