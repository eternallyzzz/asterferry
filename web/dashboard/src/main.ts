import { createApp } from "vue";
import App from "./App.vue";
import router from "./router";
import "./styles.css";

// The Dashboard is a Controller client. It has no per-node management API.
createApp(App).use(router).mount("#root");
