<script>
import AjaxLoader from "../AjaxLoader.vue";
import axios from "axios";
import commonMixins from "../../mixins/CommonMixins";
import DOMPurify from "dompurify";
import { markRaw } from "vue";
import { mailbox } from "../../stores/mailbox";
import Quill from "quill";

export default {
	components: {
		AjaxLoader,
	},

	mixins: [commonMixins],

	props: {
		message: {
			type: Object,
			default: () => null,
		},
		mode: {
			type: String,
			default: "compose",
		},
	},

	emits: ["delete"],

	data() {
		return {
			mailbox,
			from: "",
			toInput: "",
			ccInput: "",
			bccInput: "",
			subject: "",
			htmlBody: "",
			textBody: "",
			quill: null,
			originalMessageID: "",
			sending: false,
			attachments: [],
			imageProcessing: false,
		};
	},

	mounted() {
		if (this.mode === "reply" || this.mode === "replyAll") {
			this.prefillReply();
		}

		// Set content in editor div before Quill init (Quill picks it up)
		const editor = document.getElementById("ComposeEditor");
		if (editor && this.htmlBody) {
			editor.innerHTML = this.htmlBody;
		}

		// Initialize Quill after the modal is fully shown
		const modalEl = document.getElementById("ComposeModal");
		if (modalEl) {
			modalEl.addEventListener("shown.bs.modal", () => {
				this.$nextTick(() => {
					this.initQuill();
				});
			});
		}
	},

	beforeUnmount() {
		if (this.quill) {
			this.quill = null;
		}
	},

	methods: {

		prefillReply() {
			if (!this.message) return;

			const originalFrom = this.message.From;
			const originalReplyTo = this.message.ReplyTo;
			const originalTo = this.message.To || [];
			const originalCc = this.message.Cc || [];

			// From: original To (first recipient) — we are replying as the recipient
			if (originalTo.length > 0) {
				this.from = this.formatAddress(originalTo[0]);
			}

			// To: original Reply-To (if set), otherwise original From (RFC 5322)
			const toList = [];
			const replyTarget = originalReplyTo && originalReplyTo.length > 0 ? originalReplyTo[0] : originalFrom;
			if (replyTarget) {
				toList.push(this.formatAddress(replyTarget));
			}

			// Reply All: also include To and Cc except our own from address
			if (this.mode === "replyAll") {
				const excludeAddr = this.from ? this.from.split("<")[1]?.replace(">", "").trim().toLowerCase() : "";
				for (let i = 1; i < originalTo.length; i++) {
					const addr = this.formatAddress(originalTo[i]);
					const addrEmail = originalTo[i].Address.toLowerCase();
					if (addrEmail !== excludeAddr) {
						toList.push(addr);
					}
				}
				const ccList = [];
				for (const i in originalCc) {
					const addr = this.formatAddress(originalCc[i]);
					const addrEmail = originalCc[i].Address.toLowerCase();
					if (addrEmail !== excludeAddr) {
						ccList.push(addr);
					}
				}
				this.ccInput = ccList.join(", ");
			}

			this.toInput = toList.join(", ");

			// Subject
			if (this.message.Subject) {
				if (!this.message.Subject.match(/^Re:/i)) {
					this.subject = "Re: " + this.message.Subject;
				} else {
					this.subject = this.message.Subject;
				}
			}

			// Store original Message-ID for threading headers (RFC 5322 requires angle brackets)
			if (this.message.MessageID) {
				this.originalMessageID = "<" + this.message.MessageID + ">";
			}

			// Build quote from original message
			let quoteText = "";
			if (this.message.Text) {
				const lines = this.message.Text.split("\n");
				for (const i in lines) {
					quoteText += "> " + lines[i] + "\n";
				}
			}

			const fromStr = originalFrom ? this.formatAddress(originalFrom) : "unknown";
			const dateStr = this.message.Date ? new Date(this.message.Date).toLocaleString() : "unknown";

			const quotedFromLine =
				"<p><strong>" +
				this.escapeHtml(fromStr) +
				"</strong> wrote on <em>" +
				this.escapeHtml(dateStr) +
				"</em>:</p>";

			if (this.message.HTML) {
				const sanitized = DOMPurify.sanitize(this.message.HTML, {
					WHOLE_DOCUMENT: true,
					FORBID_TAGS: ["script", "form"],
					ALLOW_UNKNOWN_PROTOCOLS: true,
				});
				this.htmlBody =
					"<br><br><blockquote style='border-left:2px solid #ccc;margin:0;padding:0 0 0 8px'>" +
					quotedFromLine +
					"<div style='all:initial;background:#fff;color:#000;padding:8px;font-family:Arial,sans-serif;font-size:14px;line-height:1.4'>" +
					sanitized +
					"</div></blockquote>";
			} else if (quoteText) {
				this.textBody = "\n\n\n--- " + fromStr + " wrote on " + dateStr + " ---\n" + quoteText;
				this.htmlBody =
					"<br><br><blockquote style='border-left:2px solid #ccc;margin:0;padding:0 0 0 8px'>" +
					quotedFromLine +
					"<pre style='background:#fff;color:#000;padding:8px;font-family:monospace;font-size:13px'>" +
					this.escapeHtml(this.message.Text) +
					"</pre></blockquote>";
			}
		},

		initQuill() {
			const editor = document.getElementById("ComposeEditor");
			if (!editor || this.quill) return;

			this.quill = markRaw(new Quill(editor, {
				theme: "snow",
				modules: {
					toolbar: {
						container: [
							[{ header: [1, 2, 3, false] }],
							["bold", "italic", "underline", "strike"],
							[{ list: "ordered" }, { list: "bullet" }],
							["blockquote", "code-block"],
							["link", "image"],
							["clean"],
						],
						handlers: {
							image: () => this.quillImageHandler(),
						},
					},
				},
				placeholder: "Compose your message...",
			}));
		},

		getQuillHTML() {
			if (!this.quill) return "";
			return this.quill.root.innerHTML;
		},

		getQuillText() {
			if (!this.quill) return "";
			return this.quill.getText();
		},

		formatAddress(addr) {
			if (!addr) return "";
			if (addr.Name && addr.Name !== addr.Address) {
				return addr.Name + " <" + addr.Address + ">";
			}
			return addr.Address;
		},

		escapeHtml(str) {
			const div = document.createElement("div");
			div.appendChild(document.createTextNode(str));
			return div.innerHTML;
		},

		async sendMessage() {
			if (this.sending) return;
			this.sending = true;
			this.loading++;

			const html = this.getQuillHTML();
			const text = this.getQuillText();

			if (!this.from) {
				alert("Please enter a From address.");
				this.sending = false;
				this.loading--;
				return;
			}

			const toList = this.parseAddressList(this.toInput);
			if (!toList.length) {
				alert("Please enter at least one recipient.");
				this.sending = false;
				this.loading--;
				return;
			}

			const ccList = this.parseAddressList(this.ccInput);
			const bccList = this.bccInput
				.split(",")
				.map((a) => a.trim())
				.filter((a) => a);

			const sendPayload = {
				From: this.parseAddressInput(this.from),
				To: toList,
				Cc: ccList,
				Bcc: bccList,
				Subject: this.subject,
				HTML: html,
				Text: text,
				Tags: ["outgoing"],
				Attachments: this.attachments.map((a) => ({
					Content: a.dataUrl,
					Filename: a.name,
					ContentType: a.file.type || "",
				})),
			};

			// Add threading headers for replies (RFC 5322)
			if (this.originalMessageID) {
				sendPayload.Headers = {
					"In-Reply-To": this.originalMessageID,
					"References": this.originalMessageID,
				};
			}

			try {
				// Step 1: Save message to database via send API
				const sendResp = await axios.post(this.resolve("/api/v1/send"), sendPayload);
				const msgID = sendResp.data.ID;

				// Collect all recipient addresses for relay
				const allRecipients = [];
				for (const a of toList) {
					if (a.Email) allRecipients.push(a.Email);
				}
				for (const a of ccList) {
					if (a.Email) allRecipients.push(a.Email);
				}
				for (const a of bccList) {
					if (a) allRecipients.push(a);
				}

				// Step 2: Relay via SMTP if there are recipients and a valid ID
				if (allRecipients.length && msgID) {
					await axios.post(this.resolve("/api/v1/message/" + msgID + "/release"), { To: allRecipients });
				}

				// Mark sent message as read (we sent it, so it's not unread)
				if (msgID) {
					await axios.put(this.resolve("/api/v1/messages"), { Read: true, IDs: [msgID] });
				}

				this.sending = false;
				this.modal("ComposeModal").hide();
			} catch (error) {
				this.sending = false;
				let msg = "Error sending message.";
				if (error.response && error.response.data) {
					if (error.response.data.Error) {
						msg = error.response.data.Error;
					} else if (typeof error.response.data === "string") {
						msg = error.response.data;
					}
				} else if (error.request) {
					msg = "Error sending data to the server. Please try again.";
				} else if (error.message) {
					msg = error.message;
				}
				alert(msg);
			} finally {
				if (this.loading > 0) this.loading--;
			}
		},

		parseAddressInput(str) {
			const s = str.trim();
			const match = s.match(/^(.*?)\s*<([^>]+)>$/);
			if (match) {
				return { Name: match[1].trim(), Email: match[2].trim() };
			}
			return { Name: "", Email: s };
		},

		parseAddressList(str) {
			return str
				.split(",")
				.map((a) => this.parseAddressInput(a))
				.filter((a) => a.Email);
		},

		// triggered manually after modal is shown
		initTags() {
			// Address fields are simple text inputs for compose
		},

		quillImageHandler() {
			const index = this.quill.getLength();

			const input = document.createElement("input");
			input.type = "file";
			input.accept = "image/*";
			input.multiple = false;
			input.onchange = () => {
				const file = input.files[0];
				if (!file) return;

				this.imageProcessing = true;
				const reader = new FileReader();
				reader.onload = (e) => {
					this.quill.insertEmbed(index, "image", e.target.result, "silent");
					this.quill.setSelection(index + 1, 0, "silent");
					this.imageProcessing = false;
				};
				reader.readAsDataURL(file);
			};
			input.click();
		},

		handleAttachmentUpload(event) {
			const files = event.target.files;
			for (const file of files) {
				const reader = new FileReader();
				reader.onload = (e) => {
					this.attachments.push({
						file,
						name: file.name,
						size: file.size,
						dataUrl: e.target.result,
					});
				};
				reader.readAsDataURL(file);
			}
			event.target.value = "";
		},

		removeAttachment(index) {
			this.attachments.splice(index, 1);
		},

		formatFileSize(bytes) {
			if (bytes === 0) return "0 B";
			const units = ["B", "kB", "MB", "GB"];
			const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
			return (bytes / Math.pow(1024, i)).toFixed(1) + " " + units[i];
		},
	},
};
</script>

<template>
	<div
		id="ComposeModal"
		class="modal fade"
		tabindex="-1"
		aria-labelledby="ComposeModalLabel"
		aria-hidden="true"
		data-bs-backdrop="static"
	>
		<div class="modal-dialog modal-xl modal-fullscreen-lg-down">
			<div class="modal-content">
				<div class="modal-header">
					<h1 id="ComposeModalLabel" class="modal-title fs-5">
						<template v-if="mode === 'reply' || mode === 'replyAll'">
							{{ mode === "replyAll" ? "Reply All" : "Reply" }}
						</template>
						<template v-else>New Message</template>
					</h1>
					<button type="button" class="btn-close" data-bs-dismiss="modal" aria-label="Close"></button>
				</div>
				<div class="modal-body">
					<div class="mb-2 row">
						<label class="col-sm-2 col-form-label text-body-secondary">From</label>
						<div class="col-sm-10">
							<input
								v-model="from"
								type="text"
								class="form-control form-control-sm"
								placeholder="sender@example.com"
								:readonly="mode !== 'compose' && mailbox.uiConfig.MessageRelay && mailbox.uiConfig.MessageRelay.OverrideFrom !== ''"
							/>
						</div>
					</div>
					<div class="mb-2 row">
						<label class="col-sm-2 col-form-label text-body-secondary">To</label>
						<div class="col-sm-10">
							<input
								v-model="toInput"
								type="text"
								class="form-control form-control-sm"
								placeholder="recipient@example.com"
							/>
						</div>
					</div>
					<div class="mb-2 row">
						<label class="col-sm-2 col-form-label text-body-secondary">Cc</label>
						<div class="col-sm-10">
							<input
								v-model="ccInput"
								type="text"
								class="form-control form-control-sm"
								placeholder="cc@example.com"
							/>
						</div>
					</div>
					<div class="mb-2 row">
						<label class="col-sm-2 col-form-label text-body-secondary">Bcc</label>
						<div class="col-sm-10">
							<input v-model="bccInput" type="text" class="form-control form-control-sm" placeholder="bcc@example.com" />
						</div>
					</div>
					<div class="mb-2 row">
						<label class="col-sm-2 col-form-label text-body-secondary">Subject</label>
						<div class="col-sm-10">
							<input v-model="subject" type="text" class="form-control form-control-sm" placeholder="Subject" />
						</div>
					</div>
			<div class="mb-2 row align-items-start">
				<label class="col-sm-2 col-form-label text-body-secondary">Message</label>
				<div class="col-sm-10">
					<div
						id="ComposeEditor"
						class="quill-editor"
						style="min-height: 250px; max-height: 50vh; overflow-y: auto"
					></div>
					<div v-if="imageProcessing" class="mt-1 text-body-secondary small">
						<i class="bi bi-hourglass-split me-1"></i>Processing image...
					</div>
				</div>
			</div>
			<div class="mb-2 row align-items-start">
				<label class="col-sm-2 col-form-label text-body-secondary">Attachments</label>
				<div class="col-sm-10">
					<div v-if="attachments.length" class="mb-2">
						<div
							v-for="(att, idx) in attachments"
							:key="idx"
							class="d-inline-flex align-items-center border rounded px-2 py-1 me-1 mb-1 bg-light"
						>
							<i class="bi bi-paperclip me-1 text-body-secondary"></i>
							<span class="small">{{ att.name }} ({{ formatFileSize(att.size) }})</span>
							<button
								type="button"
								class="btn-close btn-close-sm ms-2"
								style="font-size: 0.6em"
								aria-label="Remove"
								@click="removeAttachment(idx)"
							></button>
						</div>
					</div>
					<div>
						<label class="btn btn-outline-secondary btn-sm">
							<i class="bi bi-paperclip me-1"></i>Attach files
							<input
								type="file"
								multiple
								class="d-none"
								@change="handleAttachmentUpload($event)"
							/>
						</label>
						<small class="text-body-secondary ms-2">Attachments are sent as regular email attachments</small>
					</div>
				</div>
			</div>
		</div>
				<div class="modal-footer">
					<button type="button" class="btn btn-outline-secondary" data-bs-dismiss="modal">Cancel</button>
					<button type="button" class="btn btn-primary" :disabled="sending" @click="sendMessage">
						<i v-if="sending" class="bi bi-hourglass-split me-1"></i>
						<i v-else class="bi bi-send me-1"></i>
						{{ sending ? "Sending..." : "Send" }}
					</button>
				</div>
			</div>
		</div>
	</div>

	<AjaxLoader :loading="loading" />
</template>

<style scoped>
.quill-editor :deep(.ql-toolbar) {
	border-top-left-radius: 0.375rem;
	border-top-right-radius: 0.375rem;
	background: #fff;
	color: #000;
}

.quill-editor :deep(.ql-toolbar .ql-stroke) {
	stroke: #444;
}

.quill-editor :deep(.ql-toolbar .ql-fill) {
	fill: #444;
}

.quill-editor :deep(.ql-toolbar .ql-picker-label) {
	color: #444;
}

.quill-editor :deep(.ql-container) {
	border-bottom-left-radius: 0.375rem;
	border-bottom-right-radius: 0.375rem;
	min-height: 250px;
	background: #fff;
	color: #000;
}

.quill-editor :deep(.ql-editor) {
	min-height: 250px;
	background: #fff;
	color: #000;
}

.quill-editor :deep(.ql-editor.ql-blank::before) {
	color: #888;
}
</style>
