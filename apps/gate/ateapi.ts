import grpc from "@grpc/grpc-js";
import loader from "@grpc/proto-loader";

const definition = loader.loadSync(new URL("./ateapi.proto", import.meta.url).pathname, { longs: String, enums: String, oneofs: true });
const { Control } = (grpc.loadPackageDefinition(definition) as any).substrate.ateapi.v1;

type Ref = { atespace: string; name: string };
type Actor = { metadata: { name: string }; status: { state: string; workerAssignment?: { workerPod?: string } } };
type ActorTemplate = { metadata: { name: string; createTime: { seconds: string } } };
type EgressRule =
	| { tlsPassthrough: { hostnames: string[]; ports: { all: {} } } }
	| { http: { hostnames: string[] } }
	| { https: { hostnames: string[]; effects: { replaceHeaders: { header: string; prefix: string; credentialUri: string }[] } } };

// The Control methods the gate calls: what each takes, and what the gate reads of its reply.
type Methods = {
	ListActorTemplates: [{ atespace: string }, { actorTemplates?: ActorTemplate[] }];
	ListActors: [{ atespace: string }, { actors?: Actor[] }];
	GetActor: [{ actor: Ref }, Actor];
	CreateActor: [{ actor: { metadata: Ref; actorTemplate: Ref } }, void];
	CreateActorEgressPolicy: [{ actor: Ref; egressPolicy: { metadata: Ref; rules: EgressRule[] } }, void];
	ResumeActor: [{ actor: Ref }, void];
	SuspendActor: [{ actor: Ref; options: { mode: string } }, void];
	DeleteActor: [{ actor: Ref }, void];
};
export type Method = keyof Methods;
export type Request<M extends Method> = Methods[M][0];
export type Reply<M extends Method> = Methods[M][1];

export type Api = <M extends Method>(method: M, request: Request<M>, token: string) => Promise<Reply<M>>;

// Long enough for a synchronous suspend to upload its snapshot.
const DEADLINE_MS = 5 * 60_000;

export function connect(address: string, ca: Buffer, serverName: string): Api {
	const client = new Control(address, grpc.credentials.createSsl(ca), { "grpc.ssl_target_name_override": serverName, "grpc.default_authority": serverName });
	return <M extends Method>(method: M, request: Request<M>, token: string) =>
		new Promise<Reply<M>>((resolve, reject) => {
			const metadata = new grpc.Metadata();
			metadata.set("authorization", `Bearer ${token}`);
			client[method](request, metadata, { deadline: Date.now() + DEADLINE_MS }, (error: grpc.ServiceError | null, reply: Reply<M>) =>
				error ? reject(error) : resolve(reply),
			);
		});
}

const HTTP_STATUS = new Map<number, number>([
	[grpc.status.INVALID_ARGUMENT, 400],
	[grpc.status.NOT_FOUND, 404],
	[grpc.status.ALREADY_EXISTS, 409],
	[grpc.status.PERMISSION_DENIED, 403],
	[grpc.status.RESOURCE_EXHAUSTED, 503],
	[grpc.status.FAILED_PRECONDITION, 409],
	[grpc.status.UNAUTHENTICATED, 401],
]);

export function httpStatus(error: { code?: number }): number {
	return HTTP_STATUS.get(error.code ?? -1) ?? 502;
}
