import { createApp } from "vue";
import "@arco-design/web-vue/dist/arco.css";
import App from "./App.vue";
import "./styles.css";

// The Dashboard is a Controller client.  Do not mount the retired per-node
// management router here: loading it would pull the old config API and token
// gate into the production bundle even though App renders ControllerShell.
createApp(App).mount("#root");
