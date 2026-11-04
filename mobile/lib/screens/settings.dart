import 'package:flutter/material.dart';

import '../api.dart';
import '../autoupload.dart';
import '../storage.dart';
import 'login.dart';

/// Настройки: учётная запись, место, автозагрузка фото, выход.
class SettingsScreen extends StatefulWidget {
  const SettingsScreen({super.key, required this.api});
  final CloudBoxApi api;
  @override
  State<SettingsScreen> createState() => _SettingsScreenState();
}

class _SettingsScreenState extends State<SettingsScreen> {
  User? _me;
  bool _auto = false;
  String? _status;

  @override
  void initState() {
    super.initState();
    widget.api.me().then((u) => mounted ? setState(() => _me = u) : null).catchError((_) {});
    AutoUpload.enabled().then((v) => mounted ? setState(() => _auto = v) : null);
  }

  Future<void> _toggle(bool on) async {
    if (!on) {
      await AutoUpload.disable();
      setState(() => _auto = false);
      return;
    }
    final all = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Автозагрузка фото'),
        content: const Text('Фото и видео будут загружаться в папку «Фото/Камера» при каждом открытии приложения.\n\n'
            'Загрузить и уже снятые фото или только новые?'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(ctx), child: const Text('Отмена')),
          TextButton(onPressed: () => Navigator.pop(ctx, false), child: const Text('Только новые')),
          FilledButton(onPressed: () => Navigator.pop(ctx, true), child: const Text('Все')),
        ],
      ),
    );
    if (all == null) return;
    if (!await AutoUpload.enable(allExisting: all)) {
      setState(() => _status = 'Нет доступа к фото — разрешите его в настройках телефона');
      return;
    }
    setState(() => _auto = true);
    _run();
  }

  Future<void> _run() async {
    setState(() => _status = 'Ищу новые фото…');
    final r = await AutoUpload.run(widget.api, status: (s) => mounted ? setState(() => _status = s) : null);
    if (!mounted) return;
    setState(() => _status = r.message ??
        (r.uploaded + r.failed == 0 ? 'Новых фото нет' : 'Загружено: ${r.uploaded}${r.failed > 0 ? ', не удалось: ${r.failed}' : ''}'));
  }

  Future<void> _logout() async {
    await Storage.clear();
    if (!mounted) return;
    Navigator.of(context).pushAndRemoveUntil(MaterialPageRoute(builder: (_) => const LoginScreen()), (_) => false);
  }

  @override
  Widget build(BuildContext context) {
    final u = _me;
    return Scaffold(
      appBar: AppBar(title: const Text('Настройки')),
      body: ListView(children: [
        ListTile(
          leading: const Icon(Icons.person),
          title: Text(u?.username ?? widget.api.user),
          subtitle: Text(widget.api.base.toString()),
        ),
        if (u != null)
          ListTile(
            leading: const Icon(Icons.storage),
            title: Text('Занято ${formatSize(u.usedBytes)}${u.quotaBytes > 0 ? ' из ${formatSize(u.quotaBytes)}' : ''}'),
            subtitle: u.quotaBytes > 0 ? LinearProgressIndicator(value: (u.usedBytes / u.quotaBytes).clamp(0, 1).toDouble()) : null,
          ),
        if (widget.api.insecure)
          const ListTile(leading: Icon(Icons.warning, color: Colors.orange), title: Text('Подключение без HTTPS'),
              subtitle: Text('Пароль и файлы передаются открытым текстом — используйте только в домашней сети')),
        const Divider(),
        SwitchListTile(
          secondary: const Icon(Icons.photo_library),
          title: const Text('Автозагрузка фото и видео'),
          subtitle: const Text('В «Фото/Камера/<год>» при открытии приложения'),
          value: _auto,
          onChanged: _toggle,
        ),
        if (_auto) ListTile(leading: const Icon(Icons.sync), title: const Text('Загрузить сейчас'), onTap: _run),
        if (_status != null) Padding(padding: const EdgeInsets.symmetric(horizontal: 16), child: Text(_status!)),
        const Divider(),
        ListTile(leading: const Icon(Icons.logout), title: const Text('Выйти'), onTap: _logout),
      ]),
    );
  }
}
