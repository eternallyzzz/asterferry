import { createApp } from "vue";
import "@arco-design/web-vue/dist/arco.css";
import App from "./App.vue";
import "./styles.css";

// The Dashboard is a Controller client. It has no per-node management API.
createApp(App).mount("#root");
