import {useEffect, useState} from "react";
import {apiFetch, apiFetchJSON} from "../api/client";
import type {ReceiverFile, ReceiverSnapshot} from "../api/types";
import {StatusChip} from "./StatusChip";
import {ConfirmDialog} from "./ConfirmDialog";
import {encodePathKey, fmtSize, fmtTime, hasTime} from "../lib/format";
import AppBar from "@mui/material/AppBar";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import CloseIcon from "@mui/icons-material/Close";
import Dialog from "@mui/material/Dialog";
import Grid from "@mui/material/Grid";
import IconButton from "@mui/material/IconButton";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import Toolbar from "@mui/material/Toolbar";
import Typography from "@mui/material/Typography";

interface ReceiversSectionProps {
    receivers: ReceiverSnapshot[];
    canDownload: boolean;
}

function startDownload(id: string, key: string) {
    const url = "/api/receivers/" + encodeURIComponent(id) + "/download/" + encodePathKey(key);

    apiFetch(url, {method: "POST"})
        .then((r) => r.json())
        .then((data: { ticket: string }) => {
            window.location.href = url + "?ticket=" + encodeURIComponent(data.ticket);
        })
        .catch(() => {
        });
}

function FileListDialog({
                            id,
                            canDownload,
                            onClose,
                            onDownload,
                        }: {
    id: string | null;
    canDownload: boolean;
    onClose: () => void;
    onDownload: (id: string, key: string) => void;
}) {
    const [files, setFiles] = useState<ReceiverFile[] | null>(null);

    useEffect(() => {
        if (!id) return;

        let cancelled = false;
        setFiles(null);

        apiFetchJSON<ReceiverFile[]>("/api/receivers/" + encodeURIComponent(id) + "/files")
            .then((f) => {
                if (!cancelled) setFiles(f || []);
            })
            .catch(() => {
                if (!cancelled) setFiles([]);
            });

        return () => {
            cancelled = true;
        };
    }, [id]);

    return (
        <Dialog open={id !== null} onClose={onClose} fullScreen>
            <AppBar position="relative" color="default" elevation={1}>
                <Toolbar sx={{gap: 1}}>
                    <Typography variant="h6" component="div" sx={{flexGrow: 1}}>
                        Files{id ? " · " + id : ""}
                    </Typography>
                    <IconButton edge="end" onClick={onClose} aria-label="close">
                        <CloseIcon/>
                    </IconButton>
                </Toolbar>
            </AppBar>
            <Box sx={{p: 2}}>
                {files === null ? (
                    <Typography color="text.secondary">loading…</Typography>
                ) : !files.length ? (
                    <Typography color="text.secondary">no files stored</Typography>
                ) : (
                    <Table size="small">
                        <TableHead>
                            <TableRow>
                                <TableCell>Key</TableCell>
                                <TableCell>Size</TableCell>
                                <TableCell>Modified</TableCell>
                                <TableCell>Expires</TableCell>
                                <TableCell/>
                            </TableRow>
                        </TableHead>
                        <TableBody>
                            {files.map((f) => (
                                <TableRow key={f.key}>
                                    <TableCell sx={{overflowWrap: "anywhere"}}>{f.key}</TableCell>
                                    <TableCell>{fmtSize(f.size)}</TableCell>
                                    <TableCell>{fmtTime(f.mod_time)}</TableCell>
                                    <TableCell>{fmtTime(f.expires_at)}</TableCell>
                                    <TableCell>
                                        {canDownload && id ? (
                                            <Link
                                                component="button"
                                                variant="body2"
                                                underline="hover"
                                                onClick={() => onDownload(id, f.key)}
                                            >
                                                download
                                            </Link>
                                        ) : null}
                                    </TableCell>
                                </TableRow>
                            ))}
                        </TableBody>
                    </Table>
                )}
            </Box>
        </Dialog>
    );
}

export function ReceiversCard({receiver, canDownload}: { receiver: ReceiverSnapshot; canDownload: boolean }) {
    const lastSeen = hasTime(receiver.last_seen)
        ? "last received: " +
        fmtTime(receiver.last_seen)
        : "no objects received yet";

    const [openFilesFor, setOpenFilesFor] = useState<string | null>(null);
    const [pendingDownload, setPendingDownload] = useState<{ id: string; key: string } | null>(null);

    return (
        <>
            <Card variant="outlined" sx={{height: "100%"}}>
                <CardContent>
                    <Stack
                        direction="row"
                        spacing={1}
                        sx={{alignItems: "baseline", justifyContent: "space-between", mb: 0.5}}
                    >
                        <Typography sx={{fontWeight: 600}}>{receiver.id}</Typography>
                        <Stack direction="row" spacing={0.5}>
                            <StatusChip state={receiver.state}/>
                            {receiver.stale ? <StatusChip state="failed" label="stale"/> : null}
                        </Stack>
                    </Stack>
                    <Stack direction={"column"} spacing={1}>
                    <Typography variant="body2" color="text.secondary">
                        {receiver.path}
                    </Typography>
                    <Typography variant="body2" color="text.secondary">
                        Retention: {receiver.retention}
                    </Typography>
                    {receiver.stale_after && (
                        <Typography variant="body2" color="text.secondary">
                            Stale after: {receiver.stale_after }
                        </Typography>
                    )}
                    <Typography variant="body2" color="text.secondary" sx={{mb: 1}}>
                        {lastSeen}
                    </Typography>
                    </Stack>
                    {receiver.error ? (
                        <Typography variant="body2" color="error" sx={{overflowWrap: "anywhere"}}>
                            {receiver.error}
                        </Typography>
                    ) : null}
                    <Button size="small" variant="outlined" onClick={() => setOpenFilesFor(receiver.id)}>
                        Show files
                    </Button>
                </CardContent>
            </Card>
            <FileListDialog
                id={openFilesFor}
                canDownload={canDownload}
                onClose={() => setOpenFilesFor(null)}
                onDownload={(id, key) => setPendingDownload({id, key})}
            />
            <ConfirmDialog
                open={pendingDownload !== null}
                message={
                    <>
                        Download <strong>{pendingDownload?.key}</strong>?
                    </>
                }
                confirmLabel="Download"
                onConfirm={() => {
                    if (pendingDownload) startDownload(pendingDownload.id, pendingDownload.key);
                    setPendingDownload(null);
                }}
                onCancel={() => setPendingDownload(null)}
            />
        </>
    );
}

export function ReceiversSection({receivers, canDownload}: ReceiversSectionProps) {
    if (!receivers.length) {
        return (
            <Typography color="text.secondary" sx={{mt: 6, textAlign: "center"}}>
                no receivers configured
            </Typography>
        );
    }

    return (
        <>
            <Grid container spacing={2}>
                {receivers.map((rcv) => {
                    return (
                        <Grid key={rcv.id} size={{xs: 12, sm: 6, md: 4}}>
                            <ReceiversCard receiver={rcv} canDownload={canDownload}/>
                        </Grid>
                    );
                })}
            </Grid>
        </>
    );
}
