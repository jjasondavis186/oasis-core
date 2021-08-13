// Allow until oasis-core#3572.
#![allow(deprecated)]

use std::{
    collections::{BTreeMap, HashMap},
    io::Cursor,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
};

use anyhow::{anyhow, Context as AnyContext, Result};
use byteorder::{BigEndian, ReadBytesExt, WriteBytesExt};
use io_context::Context as IoContext;
use thiserror::Error;

use oasis_core_keymanager_client::{KeyManagerClient, KeyPairId};
use oasis_core_runtime::{
    common::{
        crypto::{
            hash::Hash,
            mrae::deoxysii::{DeoxysII, KEY_SIZE, NONCE_SIZE, TAG_SIZE},
        },
        key_format::KeyFormat,
        namespace::Namespace,
        version::Version,
        versioned::Versioned,
    },
    consensus::{
        address::Address,
        roothash::{Message, RegistryMessage, StakingMessage},
        staking::{Account, Delegation},
        state::staking::ImmutableState as StakingImmutableState,
    },
    rak::RAK,
    runtime_context,
    storage::{StorageContext, MKVS},
    transaction::{
        dispatcher::{Dispatcher, ExecuteBatchResult, ExecuteTxResult},
        tags::Tags,
        types::{TxnBatch, TxnCall, TxnCheckResult, TxnOutput},
        Context as TxnContext,
    },
    types::{CheckTxResult, Error as RuntimeError},
    version_from_cargo, Protocol, RpcDemux, RpcDispatcher, TxnDispatcher,
};
use simple_keymanager::trusted_policy_signers;
use simple_keyvalue_api::{
    with_api, AddEscrow, Key, KeyValue, ReclaimEscrow, Transfer, UpdateRuntime, Withdraw,
};

// This is the old runtime method dispatcher that used to be part
// of the oasis-core/runtime API.  It has been moved here since
// new runtimes should use the SDK instead.

/// Registers defined transaction methods into the transaction dispatcher.
///
/// # Examples
///
/// This macro should be invoked using a concrete API generated by `runtime_api`
/// as follows:
/// ```rust,ignore
/// with_api! {
///     register_runtime_txn_methods!(txn_dispatcher, api);
/// }
/// ```
macro_rules! register_runtime_txn_methods {
    (
        $txn_dispatcher:ident,
        $(
            pub fn $method_name:ident ( $arguments_type:ty ) -> $output_type:ty ;
        )*
    ) => {
        $(
            $txn_dispatcher.add_method(
               Method::new(
                    MethodDescriptor {
                        name: stringify!($method_name).to_owned(),
                    },
                    |args: &$arguments_type,
                     ctx: &mut oasis_core_runtime::transaction::context::Context|
                        -> ::anyhow::Result<$output_type> {
                        $method_name(args, ctx)
                    },
                )
            );
        )*
    }
}

/// Dispatch error.
#[derive(Error, Debug)]
enum DispatchError {
    #[error("method not found: {method:?}")]
    MethodNotFound { method: String },
}

/// Error indicating that performing a transaction check was successful.
#[derive(Error, Debug, Default)]
#[error("transaction check successful")]
pub struct CheckOnlySuccess(pub TxnCheckResult);

/// Custom batch handler.
///
/// A custom batch handler can be configured on the `Dispatcher` and will have
/// its `start_batch` and `end_batch` methods called at the appropriate times.
pub trait BatchHandler {
    /// Called before the first call in a batch is dispatched.
    ///
    /// The context may be mutated and will be available as read-only to all
    /// runtime calls.
    fn start_batch(&self, ctx: &mut TxnContext);

    /// Called after all calls have been dispatched.
    fn end_batch(&self, ctx: &mut TxnContext);
}

/// Custom context initializer.
pub trait ContextInitializer {
    /// Called to initialize the context.
    fn init(&self, ctx: &mut TxnContext);
}

impl<F> ContextInitializer for F
where
    F: Fn(&mut TxnContext),
{
    fn init(&self, ctx: &mut TxnContext) {
        (*self)(ctx)
    }
}

/// Custom finalizer.
pub trait Finalizer {
    /// Called to finalize transaction.
    ///
    /// This method is called after storage has been finalized so the
    /// storage context is not available and using it will panic.
    fn finalize(&self, new_storage_root: Hash);
}

impl<F> Finalizer for F
where
    F: Fn(Hash),
{
    fn finalize(&self, new_storage_root: Hash) {
        (*self)(new_storage_root)
    }
}

/// Descriptor of a runtime API method.
#[derive(Clone, Debug)]
pub struct MethodDescriptor {
    /// Method name.
    pub name: String,
}

/// Handler for a runtime method.
pub trait MethodHandler<Call, Output> {
    /// Invoke the method implementation and return a response.
    fn handle(&self, call: &Call, ctx: &mut TxnContext) -> Result<Output>;
}

impl<Call, Output, F> MethodHandler<Call, Output> for F
where
    Call: 'static,
    Output: 'static,
    F: Fn(&Call, &mut TxnContext) -> Result<Output> + 'static,
{
    fn handle(&self, call: &Call, ctx: &mut TxnContext) -> Result<Output> {
        (*self)(&call, ctx)
    }
}

/// Dispatcher for a runtime method.
pub trait MethodHandlerDispatch {
    /// Get method descriptor.
    fn get_descriptor(&self) -> &MethodDescriptor;

    /// Dispatches the given raw call.
    fn dispatch(&self, call: TxnCall, ctx: &mut TxnContext) -> Result<cbor::Value>;
}

struct MethodHandlerDispatchImpl<Call, Output> {
    /// Method descriptor.
    descriptor: MethodDescriptor,
    /// Method handler.
    handler: Box<dyn MethodHandler<Call, Output>>,
}

impl<Call, Output> MethodHandlerDispatch for MethodHandlerDispatchImpl<Call, Output>
where
    Call: cbor::Decode + 'static,
    Output: cbor::Encode + 'static,
{
    fn get_descriptor(&self) -> &MethodDescriptor {
        &self.descriptor
    }

    fn dispatch(&self, call: TxnCall, ctx: &mut TxnContext) -> Result<cbor::Value> {
        let call = cbor::from_value(call.args).context("unable to parse call arguments")?;
        let response = self.handler.handle(&call, ctx)?;

        Ok(cbor::to_value(response))
    }
}

/// Runtime method dispatcher implementation.
pub struct Method {
    /// Method dispatcher.
    dispatcher: Box<dyn MethodHandlerDispatch>,
}

impl Method {
    /// Create a new enclave method descriptor.
    pub fn new<Call, Output, Handler>(method: MethodDescriptor, handler: Handler) -> Self
    where
        Call: cbor::Decode + 'static,
        Output: cbor::Encode + 'static,
        Handler: MethodHandler<Call, Output> + 'static,
    {
        Method {
            dispatcher: Box::new(MethodHandlerDispatchImpl {
                descriptor: method,
                handler: Box::new(handler),
            }),
        }
    }

    /// Return method name.
    pub fn get_name(&self) -> &String {
        &self.dispatcher.get_descriptor().name
    }

    /// Dispatch method call.
    pub fn dispatch(&self, call: TxnCall, ctx: &mut TxnContext) -> Result<cbor::Value> {
        self.dispatcher.dispatch(call, ctx)
    }
}

/// Runtime method dispatcher.
///
/// The dispatcher is a concrete implementation of the Dispatcher trait.
/// It holds all registered runtime methods and provides an entry point
/// for their invocation.
pub struct MethodDispatcher {
    /// Registered runtime methods.
    methods: HashMap<String, Method>,
    /// Registered batch handler.
    batch_handler: Option<Box<dyn BatchHandler>>,
    /// Registered context initializer.
    ctx_initializer: Option<Box<dyn ContextInitializer>>,
    /// Registered finalizer.
    finalizer: Option<Box<dyn Finalizer>>,
    /// Abort batch flag.
    abort_batch: Option<Arc<AtomicBool>>,
}

impl MethodDispatcher {
    /// Create a new runtime method dispatcher instance.
    pub fn new() -> MethodDispatcher {
        MethodDispatcher {
            methods: HashMap::new(),
            batch_handler: None,
            ctx_initializer: None,
            finalizer: None,
            abort_batch: None,
        }
    }

    /// Register a new method in the dispatcher.
    pub fn add_method(&mut self, method: Method) {
        self.methods.insert(method.get_name().clone(), method);
    }

    /// Configure batch handler.
    pub fn set_batch_handler<H>(&mut self, handler: H)
    where
        H: BatchHandler + 'static,
    {
        self.batch_handler = Some(Box::new(handler));
    }

    /// Configure context initializer.
    pub fn set_context_initializer<I>(&mut self, initializer: I)
    where
        I: ContextInitializer + 'static,
    {
        self.ctx_initializer = Some(Box::new(initializer));
    }

    /// Configure finalizer.
    #[allow(dead_code)]
    pub fn set_finalizer<F>(&mut self, finalizer: F)
    where
        F: Finalizer + 'static,
    {
        self.finalizer = Some(Box::new(finalizer));
    }

    /// Dispatches a raw runtime check request.
    fn dispatch_check(&self, call: &Vec<u8>, ctx: &mut TxnContext) -> CheckTxResult {
        match self.dispatch_fallible(call, ctx) {
            Ok(_response) => CheckTxResult {
                error: Default::default(),
                // Deprecated method dispatcher doesn't support check tx metadata.
                meta: None,
            },
            Err(error) => match error.downcast::<CheckOnlySuccess>() {
                Ok(_check_result) => CheckTxResult {
                    error: Default::default(),
                    // Deprecated method dispatcher doesn't support check tx metadata.
                    meta: None,
                },
                Err(error) => CheckTxResult {
                    error: RuntimeError {
                        module: "".to_string(),
                        code: 1,
                        message: format!("{:#}", error),
                    },
                    meta: None,
                },
            },
        }
    }

    /// Dispatches a raw runtime invocation request.
    fn dispatch_execute(&self, call: &Vec<u8>, ctx: &mut TxnContext) -> ExecuteTxResult {
        let rsp = match self.dispatch_fallible(call, ctx) {
            Ok(response) => TxnOutput::Success(response),
            Err(error) => TxnOutput::Error(format!("{:#}", error)),
        };

        ExecuteTxResult {
            output: cbor::to_vec(rsp),
            tags: ctx.take_tags(),
        }
    }

    fn dispatch_fallible(&self, call: &Vec<u8>, ctx: &mut TxnContext) -> Result<cbor::Value> {
        let call: TxnCall = cbor::from_slice(call).context("unable to parse call")?;

        match self.methods.get(&call.method) {
            Some(dispatcher) => dispatcher.dispatch(call, ctx),
            None => Err(DispatchError::MethodNotFound {
                method: call.method,
            }
            .into()),
        }
    }
}

impl Dispatcher for MethodDispatcher {
    fn check_batch(
        &self,
        mut ctx: TxnContext,
        batch: &TxnBatch,
    ) -> Result<Vec<CheckTxResult>, RuntimeError> {
        if let Some(ref ctx_init) = self.ctx_initializer {
            ctx_init.init(&mut ctx);
        }

        // Invoke start batch handler.
        if let Some(ref handler) = self.batch_handler {
            handler.start_batch(&mut ctx);
        }

        // Process batch.
        let mut results = Vec::new();
        for call in batch.iter() {
            if self
                .abort_batch
                .as_ref()
                .map(|b| b.load(Ordering::SeqCst))
                .unwrap_or(false)
            {
                return Err(RuntimeError::new("rhp/dispatcher", 1, "batch aborted"));
            }
            results.push(self.dispatch_check(call, &mut ctx));
            let _ = ctx.take_tags();
        }

        Ok(results)
    }

    fn execute_batch(
        &self,
        mut ctx: TxnContext,
        batch: &TxnBatch,
    ) -> Result<ExecuteBatchResult, RuntimeError> {
        if let Some(ref ctx_init) = self.ctx_initializer {
            ctx_init.init(&mut ctx);
        }

        // Invoke start batch handler.
        if let Some(ref handler) = self.batch_handler {
            handler.start_batch(&mut ctx);
        }

        // Process batch.
        let mut results = Vec::new();
        for call in batch.iter() {
            if self
                .abort_batch
                .as_ref()
                .map(|b| b.load(Ordering::SeqCst))
                .unwrap_or(false)
            {
                return Err(RuntimeError::new("rhp/dispatcher", 1, "batch aborted"));
            }
            results.push(self.dispatch_execute(call, &mut ctx));
        }

        // Invoke end batch handler.
        if let Some(ref handler) = self.batch_handler {
            handler.end_batch(&mut ctx);
        }

        Ok(ExecuteBatchResult {
            results,
            messages: ctx.close(),
            // No support for block tags in the deprecated dispatcher.
            block_tags: Tags::new(),
            // No support for custom batch weight limits.
            batch_weight_limits: None,
        })
    }

    fn finalize(&self, new_storage_root: Hash) {
        if let Some(ref finalizer) = self.finalizer {
            finalizer.finalize(new_storage_root);
        }
    }

    /// Configure abort batch flag.
    fn set_abort_batch_flag(&mut self, abort_batch: Arc<AtomicBool>) {
        self.abort_batch = Some(abort_batch);
    }
}

/// Key format used for transaction artifacts.
#[derive(Debug)]
struct PendingMessagesKeyFormat {
    index: u32,
}

impl KeyFormat for PendingMessagesKeyFormat {
    fn prefix() -> u8 {
        0x00
    }

    fn size() -> usize {
        4
    }

    fn encode_atoms(self, atoms: &mut Vec<Vec<u8>>) {
        let mut index: Vec<u8> = Vec::with_capacity(4);
        index.write_u32::<BigEndian>(self.index).unwrap();
        atoms.push(index);
    }

    fn decode_atoms(data: &[u8]) -> Self {
        let mut reader = Cursor::new(data);
        Self {
            index: reader.read_u32::<BigEndian>().unwrap(),
        }
    }
}

/// Key format used for transaction nonces.
#[derive(Debug)]
struct NonceKeyFormat {
    nonce: u64,
}

impl KeyFormat for NonceKeyFormat {
    fn prefix() -> u8 {
        0xFF
    }

    fn size() -> usize {
        8
    }

    fn encode_atoms(self, atoms: &mut Vec<Vec<u8>>) {
        let mut nonce: Vec<u8> = Vec::with_capacity(8);
        nonce.write_u64::<BigEndian>(self.nonce).unwrap();
        atoms.push(nonce);
    }

    fn decode_atoms(data: &[u8]) -> Self {
        let mut reader = Cursor::new(data);
        Self {
            nonce: reader.read_u64::<BigEndian>().unwrap(),
        }
    }
}

struct Context {
    test_runtime_id: Namespace,
    km_client: Arc<dyn KeyManagerClient>,
}

/// Return previously set runtime ID of this runtime.
fn get_runtime_id(_args: &(), ctx: &mut TxnContext) -> Result<Option<String>> {
    let rctx = runtime_context!(ctx, Context);

    Ok(Some(rctx.test_runtime_id.to_string()))
}

fn check_nonce(nonce: u64, ctx: &mut TxnContext) -> Result<()> {
    let nonce_key = NonceKeyFormat { nonce: nonce }.encode();
    StorageContext::with_current(|mkvs, _untrusted_local| {
        match mkvs.get(IoContext::create_child(&ctx.io_ctx), &nonce_key) {
            Some(_) => Err(anyhow!("Duplicate nonce: {}", nonce)),
            None => {
                if !ctx.check_only {
                    mkvs.insert(IoContext::create_child(&ctx.io_ctx), &nonce_key, &[0x1]);
                }
                Ok(())
            }
        }
    })
}

/// Queries all consensus accounts.
/// Note: this is a transaction but could be a query in a non-test runtime.
fn consensus_accounts(
    _args: &(),
    ctx: &mut TxnContext,
) -> Result<(
    BTreeMap<Address, Account>,
    BTreeMap<Address, BTreeMap<Address, Delegation>>,
)> {
    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }

    let state = StakingImmutableState::new(&ctx.consensus_state);
    let mut result = BTreeMap::new();
    let addrs = state.addresses(IoContext::create_child(&ctx.io_ctx))?;
    for addr in addrs {
        result.insert(
            addr.clone(),
            state.account(IoContext::create_child(&ctx.io_ctx), addr)?,
        );
    }

    let delegations = state.delegations(IoContext::create_child(&ctx.io_ctx))?;

    Ok((result, delegations))
}

/// Withdraw from the consensus layer into the runtime account.
fn consensus_withdraw(args: &Withdraw, ctx: &mut TxnContext) -> Result<()> {
    check_nonce(args.nonce, ctx)?;

    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }

    StorageContext::with_current(|mkvs, _untrusted_local| {
        let index = ctx.emit_message(Message::Staking(Versioned::new(
            0,
            StakingMessage::Withdraw(args.withdraw.clone()),
        )));

        mkvs.insert(
            IoContext::create_child(&ctx.io_ctx),
            &PendingMessagesKeyFormat { index }.encode(),
            b"withdraw",
        );
    });

    Ok(())
}

/// Transfer from the runtime account to another account in the consensus layer.
fn consensus_transfer(args: &Transfer, ctx: &mut TxnContext) -> Result<()> {
    check_nonce(args.nonce, ctx)?;

    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }

    StorageContext::with_current(|mkvs, _untrusted_local| {
        let index = ctx.emit_message(Message::Staking(Versioned::new(
            0,
            StakingMessage::Transfer(args.transfer.clone()),
        )));

        mkvs.insert(
            IoContext::create_child(&ctx.io_ctx),
            &PendingMessagesKeyFormat { index }.encode(),
            b"transfer",
        );
    });

    Ok(())
}

/// Add escrow from the runtime account to an account in the consensus layer.
fn consensus_add_escrow(args: &AddEscrow, ctx: &mut TxnContext) -> Result<()> {
    check_nonce(args.nonce, ctx)?;

    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }

    StorageContext::with_current(|mkvs, _untrusted_local| {
        let index = ctx.emit_message(Message::Staking(Versioned::new(
            0,
            StakingMessage::AddEscrow(args.escrow.clone()),
        )));

        mkvs.insert(
            IoContext::create_child(&ctx.io_ctx),
            &PendingMessagesKeyFormat { index }.encode(),
            b"add_escrow",
        );
    });

    Ok(())
}

/// Reclaim escrow to the runtime account.
fn consensus_reclaim_escrow(args: &ReclaimEscrow, ctx: &mut TxnContext) -> Result<()> {
    check_nonce(args.nonce, ctx)?;

    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }

    StorageContext::with_current(|mkvs, _untrusted_local| {
        let index = ctx.emit_message(Message::Staking(Versioned::new(
            0,
            StakingMessage::ReclaimEscrow(args.reclaim_escrow.clone()),
        )));

        mkvs.insert(
            IoContext::create_child(&ctx.io_ctx),
            &PendingMessagesKeyFormat { index }.encode(),
            b"reclaim_escrow",
        );
    });

    Ok(())
}

/// Update existing runtime with given descriptor.
fn update_runtime(args: &UpdateRuntime, ctx: &mut TxnContext) -> Result<()> {
    check_nonce(args.nonce, ctx)?;

    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }

    StorageContext::with_current(|mkvs, _untrusted_local| {
        let index = ctx.emit_message(Message::Registry(Versioned::new(
            0,
            RegistryMessage::UpdateRuntime(args.update_runtime.clone()),
        )));

        mkvs.insert(
            IoContext::create_child(&ctx.io_ctx),
            &PendingMessagesKeyFormat { index }.encode(),
            b"update_runtime",
        );
    });

    Ok(())
}

/// Insert a key/value pair.
fn insert(args: &KeyValue, ctx: &mut TxnContext) -> Result<Option<String>> {
    check_nonce(args.nonce, ctx)?;

    if args.value.as_bytes().len() > 128 {
        return Err(anyhow!("Value too big to be inserted."));
    }
    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }
    ctx.emit_txn_tag(b"kv_op", b"insert");
    ctx.emit_txn_tag(b"kv_key", args.key.as_bytes());

    let existing = StorageContext::with_current(|mkvs, _untrusted_local| {
        mkvs.insert(
            IoContext::create_child(&ctx.io_ctx),
            args.key.as_bytes(),
            args.value.as_bytes(),
        )
    });
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// Retrieve a key/value pair.
fn get(args: &Key, ctx: &mut TxnContext) -> Result<Option<String>> {
    check_nonce(args.nonce, ctx)?;

    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }
    ctx.emit_txn_tag(b"kv_op", b"get");
    ctx.emit_txn_tag(b"kv_key", args.key.as_bytes());

    let existing = StorageContext::with_current(|mkvs, _untrusted_local| {
        mkvs.get(IoContext::create_child(&ctx.io_ctx), args.key.as_bytes())
    });
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// Remove a key/value pair.
fn remove(args: &Key, ctx: &mut TxnContext) -> Result<Option<String>> {
    check_nonce(args.nonce, ctx)?;

    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }
    ctx.emit_txn_tag(b"kv_op", b"remove");
    ctx.emit_txn_tag(b"kv_key", args.key.as_bytes());

    let existing = StorageContext::with_current(|mkvs, _untrusted_local| {
        mkvs.remove(IoContext::create_child(&ctx.io_ctx), args.key.as_bytes())
    });
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// Helper for doing encrypted MKVS operations.
fn get_encryption_context(ctx: &mut TxnContext, key: &[u8]) -> Result<EncryptionContext> {
    let rctx = runtime_context!(ctx, Context);

    // Derive key pair ID based on key.
    let key_pair_id = KeyPairId::from(Hash::digest_bytes(key).as_ref());

    // Fetch encryption keys.
    let io_ctx = IoContext::create_child(&ctx.io_ctx);
    let result = rctx.km_client.get_or_create_keys(io_ctx, key_pair_id);
    let key = ctx.tokio.block_on(result)?;

    Ok(EncryptionContext::new(key.state_key.as_ref()))
}

/// (encrypted) Insert a key/value pair.
fn enc_insert(args: &KeyValue, ctx: &mut TxnContext) -> Result<Option<String>> {
    check_nonce(args.nonce, ctx)?;

    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }
    // NOTE: This is only for example purposes, the correct way would be
    //       to also generate a (deterministic) nonce.
    let nonce = [0u8; NONCE_SIZE];

    let enc_ctx = get_encryption_context(ctx, args.key.as_bytes())?;
    let existing = StorageContext::with_current(|mkvs, _untrusted_local| {
        enc_ctx.insert(
            mkvs,
            IoContext::create_child(&ctx.io_ctx),
            args.key.as_bytes(),
            args.value.as_bytes(),
            &nonce,
        )
    });
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// (encrypted) Retrieve a key/value pair.
fn enc_get(args: &Key, ctx: &mut TxnContext) -> Result<Option<String>> {
    check_nonce(args.nonce, ctx)?;

    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }
    let enc_ctx = get_encryption_context(ctx, args.key.as_bytes())?;
    let existing = StorageContext::with_current(|mkvs, _untrusted_local| {
        enc_ctx.get(
            mkvs,
            IoContext::create_child(&ctx.io_ctx),
            args.key.as_bytes(),
        )
    });
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// (encrypted) Remove a key/value pair.
fn enc_remove(args: &Key, ctx: &mut TxnContext) -> Result<Option<String>> {
    check_nonce(args.nonce, ctx)?;

    if ctx.check_only {
        return Err(CheckOnlySuccess::default().into());
    }
    let enc_ctx = get_encryption_context(ctx, args.key.as_bytes())?;
    let existing = StorageContext::with_current(|mkvs, _untrusted_local| {
        enc_ctx.remove(
            mkvs,
            IoContext::create_child(&ctx.io_ctx),
            args.key.as_bytes(),
        )
    });
    Ok(existing.map(|v| String::from_utf8(v)).transpose()?)
}

/// A keyed storage encryption context, for use with a MKVS instance.
struct EncryptionContext {
    d2: DeoxysII,
}

impl EncryptionContext {
    /// Initialize a new EncryptionContext with the given MRAE key.
    pub fn new(key: &[u8]) -> Self {
        if key.len() != KEY_SIZE {
            panic!("mkvs: invalid encryption key size {}", key.len());
        }
        let mut raw_key = [0u8; KEY_SIZE];
        raw_key.copy_from_slice(&key[..KEY_SIZE]);

        let d2 = DeoxysII::new(&raw_key);
        //raw_key.zeroize();

        Self { d2 }
    }

    /// Get encrypted MKVS entry.
    pub fn get(&self, mkvs: &dyn MKVS, ctx: IoContext, key: &[u8]) -> Option<Vec<u8>> {
        let key = self.derive_encrypted_key(key);
        let ciphertext = match mkvs.get(ctx, &key) {
            Some(ciphertext) => ciphertext,
            None => return None,
        };

        self.open(&ciphertext)
    }

    /// Insert encrypted MKVS entry.
    pub fn insert(
        &self,
        mkvs: &mut dyn MKVS,
        ctx: IoContext,
        key: &[u8],
        value: &[u8],
        nonce: &[u8],
    ) -> Option<Vec<u8>> {
        let nonce = Self::derive_nonce(&nonce);
        let mut ciphertext = self.d2.seal(&nonce, value.to_vec(), vec![]);
        ciphertext.extend_from_slice(&nonce);

        let key = self.derive_encrypted_key(key);
        let ciphertext = match mkvs.insert(ctx, &key, &ciphertext) {
            Some(ciphertext) => ciphertext,
            None => return None,
        };

        self.open(&ciphertext)
    }

    /// Remove encrypted MKVS entry.
    pub fn remove(&self, mkvs: &mut dyn MKVS, ctx: IoContext, key: &[u8]) -> Option<Vec<u8>> {
        let key = self.derive_encrypted_key(key);
        let ciphertext = match mkvs.remove(ctx, &key) {
            Some(ciphertext) => ciphertext,
            None => return None,
        };

        self.open(&ciphertext)
    }

    fn open(&self, ciphertext: &[u8]) -> Option<Vec<u8>> {
        // ciphertext || tag || nonce.
        if ciphertext.len() < TAG_SIZE + NONCE_SIZE {
            return None;
        }

        let nonce_offset = ciphertext.len() - NONCE_SIZE;
        let mut nonce = [0u8; NONCE_SIZE];
        nonce.copy_from_slice(&ciphertext[nonce_offset..]);
        let ciphertext = &ciphertext[..nonce_offset];

        let plaintext = self.d2.open(&nonce, ciphertext.to_vec(), vec![]);
        plaintext.ok()
    }

    fn derive_encrypted_key(&self, key: &[u8]) -> Vec<u8> {
        // XXX: The plan is eventually to use a lighter weight transform
        // for the key instead of a full fledged MRAE algorithm.  For now
        // approximate it with a Deoxys-II call with an all 0 nonce.

        let nonce = [0u8; NONCE_SIZE];
        // XXX: Prefix all keys by 0x01 to make sure they do not clash with pending messages.
        let mut pkey = vec![0x01];
        pkey.append(&mut self.d2.seal(&nonce, key.to_vec(), vec![]));
        pkey
    }

    fn derive_nonce(nonce: &[u8]) -> [u8; NONCE_SIZE] {
        // Just a copy for type safety.
        let mut n = [0u8; NONCE_SIZE];
        if nonce.len() != NONCE_SIZE {
            panic!("invalid nonce size: {}", nonce.len());
        }
        n.copy_from_slice(nonce);

        n
    }
}

struct BlockHandler;

impl BlockHandler {
    fn process_message_results(&self, ctx: &mut TxnContext) {
        for ev in &ctx.round_results.messages {
            // Fetch and remove message metadata.
            let meta = StorageContext::with_current(|mkvs, _| {
                mkvs.remove(
                    IoContext::create_child(&ctx.io_ctx),
                    &PendingMessagesKeyFormat { index: ev.index }.encode(),
                )
            });

            // Make sure metadata is as expected.
            match meta.as_ref().map(|v| v.as_slice()) {
                Some(b"withdraw") => {
                    // Withdraw.
                }

                Some(b"transfer") => {
                    // Transfer.
                }

                Some(b"add_escrow") => {
                    // AddEscrow.
                }

                Some(b"reclaim_escrow") => {
                    // ReclaimEscrow.
                }

                meta => panic!("unexpected message metadata: {:?}", meta),
            }
        }

        // Check if there are any leftover pending messages metadata.
        StorageContext::with_current(|mkvs, _| {
            let mut it = mkvs.iter(IoContext::create_child(&ctx.io_ctx));
            it.seek(&PendingMessagesKeyFormat { index: 0 }.encode_partial(0));
            // Either there should be no next key...
            it.next().and_then(|(key, _value)| {
                assert!(
                    // ...or the next key should be something else.
                    PendingMessagesKeyFormat::decode(&key).is_none(),
                    "leftover message metadata (some messages not processed?): key={:?}",
                    key
                );
                Some(())
            });
        })
    }
}

impl BatchHandler for BlockHandler {
    fn start_batch(&self, ctx: &mut TxnContext) {
        if ctx.check_only {
            return;
        }

        self.process_message_results(ctx);

        // Store current epoch to test consistency.
        StorageContext::with_current(|mkvs, _| {
            mkvs.insert(
                IoContext::create_child(&ctx.io_ctx),
                &[0x02],
                &ctx.epoch.to_be_bytes(),
            );
        });
    }

    fn end_batch(&self, _ctx: &mut TxnContext) {}
}

pub fn main() {
    // Initializer.
    let init = |protocol: &Arc<Protocol>,
                rak: &Arc<RAK>,
                _rpc_demux: &mut RpcDemux,
                rpc: &mut RpcDispatcher|
     -> Option<Box<dyn TxnDispatcher>> {
        let mut txn = MethodDispatcher::new();
        with_api! { register_runtime_txn_methods!(txn, api); }

        // Create the key manager client.
        let rt_id = protocol.get_runtime_id();
        let km_client = Arc::new(oasis_core_keymanager_client::RemoteClient::new_runtime(
            rt_id,
            protocol.clone(),
            rak.clone(),
            1024,
            trusted_policy_signers(),
        ));
        let initializer_km_client = km_client.clone();

        #[cfg(not(target_env = "sgx"))]
        let _ = rpc;
        #[cfg(target_env = "sgx")]
        rpc.set_keymanager_policy_update_handler(Some(Box::new(move |raw_signed_policy| {
            km_client
                .set_policy(raw_signed_policy)
                .expect("failed to update km client policy");
        })));

        txn.set_batch_handler(BlockHandler);
        txn.set_context_initializer(move |ctx: &mut TxnContext| {
            ctx.runtime = Box::new(Context {
                test_runtime_id: rt_id.clone(),
                km_client: initializer_km_client.clone(),
            })
        });

        Some(Box::new(txn))
    };

    // Start the runtime.
    oasis_core_runtime::start_runtime(Box::new(init), version_from_cargo!());
}
