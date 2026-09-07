// WildFlow optional adapter for the New API task-plugin v1 host.
// Registration, channel selection, persistence, polling and billing belong to the host.
export const meta = {
  apiVersion: 1, key: "apimart-image", name: "APIMart Images", version: "0.1.1",
  author: { name: "WildFlow" }, models: ["gpt-image-2", "gpt-image-2-official"],
  fetchMode: "per_task",
  protocols: [{ name: "openai_responses", supports: ["sync", "background"] }],
  usageSchema: { images: { type: "number", unit: "count" }, credits: { type: "number", unit: "credit" }, resolution: { enum: ["1k", "2k", "4k"] } },
};

function apiBase(baseUrl) {
  return String(baseUrl || "").replace(/\/+$/, "").replace(/\/v1$/, "") + "/v1";
}

export function buildSubmitRequest(ctx) {
  const req = ctx.requestBody;
  if (!req || typeof req !== "object" || Array.isArray(req)) throw new Error("JSON object required");
  if (!meta.models.includes(ctx.upstreamModel)) throw new Error("unsupported upstream image model");
  if (typeof req.prompt !== "string" || !req.prompt.trim()) throw new Error("prompt is required");
  const official = ctx.upstreamModel === "gpt-image-2-official";
  const n = req.n === undefined ? 1 : req.n;
  if (!Number.isInteger(n) || n < 1 || n > (official ? 4 : 1)) throw new Error("invalid image count for this model");
  if (req.image_urls !== undefined && (!Array.isArray(req.image_urls) || req.image_urls.length > (official ? 16 : 15) || req.image_urls.some(url => typeof url !== "string" || !url.trim()))) throw new Error("invalid reference images");
  const fields = ["size", "resolution", "image_urls", "nsfw_check"];
  if (official) fields.push("quality", "background", "moderation", "output_format", "output_compression", "mask_url");
  else fields.push("official_fallback");
  const body = { model: ctx.upstreamModel, prompt: req.prompt, n };
  for (const field of fields) if (req[field] !== undefined) body[field] = req[field];
  // Unsupported output semantics must not be silently ignored by the provider.
  if (req.response_format && req.response_format !== "url") throw new Error("only URL image results are supported");
  return { url: apiBase(ctx.baseUrl) + "/images/generations", method: "POST", headers: { Authorization: "Bearer " + ctx.apiKey, "Content-Type": "application/json" }, body };
}

export function parseSubmitResponse(ctx, response) {
  const body = response.body || {};
  if (response.statusCode < 200 || response.statusCode >= 300 || body.code !== 200 || !Array.isArray(body.data) || body.data.length !== 1 || typeof body.data[0].task_id !== "string" || !body.data[0].task_id.trim()) throw new Error("invalid APIMart submission response");
  return { taskId: body.data[0].task_id, taskData: body };
}

export function buildQueryRequest(ctx) {
  if (typeof ctx.taskId !== "string" || !ctx.taskId.trim()) throw new Error("upstream task id required");
  return { url: apiBase(ctx.baseUrl) + "/tasks/" + encodeURIComponent(ctx.taskId), method: "GET", headers: { Authorization: "Bearer " + ctx.apiKey } };
}

function imageUrls(body) {
  const images = body && body.data && body.data.result && body.data.result.images;
  if (!Array.isArray(images)) return [];
  const urls = [];
  for (const image of images) {
    const values = Array.isArray(image.url) ? image.url : [image.url];
    for (const url of values) if (typeof url === "string" && /^https?:\/\//i.test(url)) urls.push(url);
  }
  return urls;
}

export function parseTaskResult(ctx, body, response) {
  if (response.status < 200 || response.status >= 300 || !body || body.code !== 200 || !body.data || body.data.id !== ctx.taskId) throw new Error("invalid APIMart task response");
  const data = body.data;
  const statuses = { submitted: "SUBMITTED", pending: "QUEUED", processing: "IN_PROGRESS", completed: "SUCCESS", failed: "FAILURE", cancelled: "FAILURE" };
  const result = { status: statuses[data.status] || "UNKNOWN" };
  if (result.status === "SUCCESS" && imageUrls(body).length === 0) throw new Error("completed image task has no image result");
  if (typeof data.progress === "number" && Number.isFinite(data.progress)) result.progress = Math.max(0, Math.min(100, data.progress)) + "%";
  if (result.status === "SUCCESS") result.progress = "100%";
  if (result.status === "FAILURE") result.reason = data.status === "cancelled" ? "Image generation cancelled" : "Image generation failed";
  return result;
}

export function extractUsage(ctx) {
  const images = ctx.requestBody.n === undefined ? 1 : ctx.requestBody.n;
  const resolution = String(ctx.requestBody.resolution || "1k").toLowerCase();
  if (!["1k", "2k", "4k"].includes(resolution)) throw new Error("unsupported image resolution");
  // Official token cost is measured at completion; reserve 1 credit per image.
  // This is an estimate, not the retail price. The frozen expression settles
  // against APIMart credits_cost when the result arrives.
  return { images, resolution, credits: images };

}

export function extractUsageOnComplete(task, result, body) {
  const usage = {};
  if (result.status === "SUCCESS") usage.images = imageUrls(body).length;
  const credits = body && body.data && body.data.credits_cost;
  if (typeof credits === "number" && Number.isFinite(credits) && credits >= 0) usage.credits = credits;
  return usage;
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS") return [];
  return imageUrls(task.data).map((url, index) => ({ key: "image-" + index, type: "image" }));
}

export function buildContentRequest(ctx) {
  const urls = imageUrls(ctx.data);
  const index = urls.findIndex((url, i) => "image-" + i === ctx.artifactKey);
  if (index < 0) throw new Error("image artifact not found");
  // Host applies SSRF/redirect checks. Never forward the provider key to its CDN.
  return { url: urls[index], method: ctx.clientRequest.method, credentialless: true };
}

export const protocols = {
  openai_responses: {
    decodeRequest(ctx) {
      if (!ctx.body || ctx.body.kind !== "json" || !ctx.body.value || Array.isArray(ctx.body.value)) throw new Error("JSON object required");
      const req = ctx.body.value;
      const texts = [];
      const images = [];
      if (typeof req.input === "string") texts.push(req.input);
      else if (Array.isArray(req.input)) {
        for (const message of req.input) {
          if (!message || typeof message !== "object" || !Array.isArray(message.content)) throw new Error("unsupported image input");
          for (const part of message.content) {
            if (part && part.type === "input_text" && typeof part.text === "string") texts.push(part.text);
            else if (part && part.type === "input_image" && typeof part.image_url === "string") images.push(part.image_url);
            else throw new Error("unsupported image input");
          }
        }
      } else throw new Error("unsupported image input");
      const prompt = texts.join("\n");
      if (!prompt.trim()) throw new Error("prompt is required");
      const requestBody = { prompt };
      if (images.length) requestBody.image_urls = images;
      for (const field of ["n", "size", "resolution", "quality", "moderation", "output_format", "output_compression", "mask_url", "nsfw_check", "official_fallback"]) {
        if (req[field] !== undefined) requestBody[field] = req[field];
      }
      // Responses.background is the host's async flag, not the image background.
      if (req.image !== undefined) {
        if (!req.image || typeof req.image !== "object" || Array.isArray(req.image)) throw new Error("image options must be an object");
        for (const field of ["n", "size", "resolution", "quality", "background", "moderation", "output_format", "output_compression", "mask_url", "nsfw_check", "official_fallback"]) {
          if (req.image[field] !== undefined) requestBody[field] = req.image[field];
        }
      }
      return { kind: "submit", model: ctx.model, action: "image_generation", requestBody };
    },
    renderFinal(ctx) {
      const images = [];
      for (const key of Object.keys(ctx.artifacts || {}).sort()) {
        const artifact = ctx.artifacts[key];
        if (artifact.type === "image" && typeof artifact.url === "string" && artifact.url) images.push({ id: key, url: artifact.url });
      }
      if (!images.length) throw new Error("image artifact URLs unavailable");
      return { output: [{ type: "message", status: "completed", role: "assistant", content: [{ type: "output_text", text: JSON.stringify({ images }), annotations: [], logprobs: [] }] }] };
    },
  },
};
