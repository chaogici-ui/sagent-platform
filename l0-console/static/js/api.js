      async function quickStart(id) {
        await fetch(API + "/action", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ agent_id: id, action: "start" }),
        });
        toast("已发送启动指令 → " + id, "ok");
        setTimeout(refresh, 3000);
      }

      // ===================================================================
      //  PAGE: Templates (Edge / Proxy config)
      // ===================================================================