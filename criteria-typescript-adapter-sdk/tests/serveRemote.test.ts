import { describe, test, expect } from "bun:test";
import * as net from "net";
import * as path from "path";
import * as fs from "fs";
import * as grpc from "@grpc/grpc-js";
import * as protoLoader from "@grpc/proto-loader";
import { serveRemote, type Service, type RemoteIdentity } from "../src/serveRemote";

function findProtoRoot(): string {
  let dir = __dirname;
  for (let i = 0; i < 5; i++) {
    const candidate = path.join(dir, "proto");
    if (fs.existsSync(path.join(candidate, "criteria", "v2", "adapter.proto"))) {
      return candidate;
    }
    const parent = path.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  throw new Error("cannot locate proto/criteria/v2/adapter.proto relative to " + __dirname);
}

const PROTO_ROOT = findProtoRoot();

const packageDefinition = protoLoader.loadSync(
  path.join(PROTO_ROOT, "criteria", "v2", "adapter.proto"),
  {
    keepCase: true,
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
    includeDirs: [PROTO_ROOT],
  }
);

const protoDescriptor = grpc.loadPackageDefinition(packageDefinition) as any;
const AdapterServiceClient = protoDescriptor.criteria.v2.AdapterService as any;

function pickSock(prefix: string): string {
  return `/tmp/criteria-ts-test-${prefix}-${process.pid}-${Date.now()}.sock`;
}

async function readLine(conn: net.Socket): Promise<string> {
  return new Promise((resolve, reject) => {
    const buf: Buffer[] = [];
    const onData = (data: Buffer) => {
      buf.push(data);
      const all = Buffer.concat(buf).toString("utf-8");
      const idx = all.indexOf("\n");
      if (idx >= 0) {
        conn.off("data", onData);
        resolve(all.slice(0, idx));
      }
    };
    conn.on("data", onData);
    conn.on("error", reject);
    conn.on("close", () => reject(new Error("closed before newline")));
  });
}

describe("serveRemote", () => {
  test("handshake and Info() round-trip over Unix socket", async () => {
    const hostSock = pickSock("host");
    const adapterSock = pickSock("adapter");

    // Host side: listen for the adapter phone-home.
    const server = net.createServer();
    await new Promise<void>((resolve) => server.listen(hostSock, resolve));

    const acceptPromise = new Promise<net.Socket>((resolve) => {
      server.once("connection", resolve);
    });

    const identity: RemoteIdentity = {
      name: "test-adapter",
      version: "1.0.0",
      digest: "sha256:abc123",
    };

    const svc: Service = {
      info(call, callback) {
        callback(null, {
          name: identity.name,
          version: identity.version,
          description: "test",
          capabilities: [],
          platforms: [],
          sdk_protocol_version: "2",
          source_url: "",
          config_schema: {},
          input_schema: {},
          output_schema: {},
          secrets: {},
          permissions: [],
          compatible_environments: [],
          container_image: "",
          supported_features: [],
          max_chunk_bytes: 0,
        });
      },
      openSession(_call, callback) { callback(null, {}); },
      execute(_call, callback) { callback(null, {}); },
      log(_call, callback) { callback(null, {}); },
      permissions(_call) { /* duplex, no-op */ },
      closeSession(_call, callback) { callback(null, {}); },
    };

    // Start adapter in background.
    const adapterPromise = serveRemote(svc, {
      host: hostSock,
      identity,
      acceptToken: "secret-token",
      socketPath: adapterSock,
    });

    // Accept adapter connection and read handshake.
    const conn = await acceptPromise;
    const line = await readLine(conn);
    const handshake = JSON.parse(line);
    expect(handshake.name).toBe("test-adapter");
    expect(handshake.version).toBe("1.0.0");
    expect(handshake.digest).toBe("sha256:abc123");
    expect(handshake.token).toBe("secret-token");
    expect(handshake.sdk_protocol_version).toBe(2);

    // Connect to the adapter's internal gRPC server directly.
    const client = new AdapterServiceClient(
      `unix://${adapterSock}`,
      grpc.credentials.createInsecure()
    );

    const infoResp: any = await new Promise((resolve, reject) => {
      client.info({}, (err: any, resp: any) => {
        if (err) reject(err);
        else resolve(resp);
      });
    });

    expect(infoResp.name).toBe("test-adapter");
    expect(infoResp.version).toBe("1.0.0");

    // Clean up.
    client.close();
    conn.destroy();
    server.close();
    await adapterPromise.catch(() => {});

    try { fs.unlinkSync(hostSock); } catch {}
    try { fs.unlinkSync(adapterSock); } catch {}
  });

  test("throws when host is missing", async () => {
    await expect(
      serveRemote(
        { info(_call, cb) { cb(null, {}); } } as any,
        { host: "", identity: { name: "a", version: "1", digest: "sha256:x" } }
      )
    ).rejects.toThrow("host is required");
  });
});
