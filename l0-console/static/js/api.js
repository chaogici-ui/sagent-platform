      async function quickStart(id) {
        try {
          var r = await fetch(API + "/action", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ agent_id: id, action: "start" }),
          });
          var d = {};
          try { d = await r.json(); } catch (e) {}
          if (!r.ok || d.error) {
            toast("启动失败 → " + id + "：" + (d.error || "HTTP " + r.status), "err");
            return;
          }
          toast("已发送启动指令 → " + id, "ok");
          setTimeout(refresh, 3000);
        } catch (e) {
          toast("启动指令发送失败 → " + id + "：" + e, "err");
        }
      }

      // ===================================================================
      //  PAGE: Templates (Edge / Proxy config)
      // ===================================================================